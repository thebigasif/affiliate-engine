package commissions_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// FR-12 tiered rates: volume-based performance tiers. A tier ladder per
// campaign replaces the BASE campaign rate once the affiliate's cumulative
// referred revenue (commissions.gross_minor) crosses a threshold.
//
// Precedence (money-true, via ProcessPurchase):
//   coupon > affiliate+product > affiliate > campaign+product >
//   volume tier (product-specific > agnostic) > campaign default
//
// seed() installed the 10% campaign default (1000 bp).

func tierPurchase(id, key string, amount int64) commissions.Event {
	ev := purchase(id, amount)
	ev.ProductKey = key
	return ev
}

func seedTier(t *testing.T, e *env, productKey string, threshold, bp int64) {
	t.Helper()
	var key any
	if productKey != "" {
		key = productKey
	}
	if _, err := e.h.DB.Exec(`INSERT INTO rate_tiers
		(campaign_id, product_key, threshold_minor, rate_type, rate_value)
		VALUES (1, ?, ?, 'percent', ?)`, key, threshold, bp); err != nil {
		t.Fatalf("seed tier: %v", err)
	}
}

func rateSnapshot(t *testing.T, e *env, cid int64) commissions.Rule {
	t.Helper()
	var snap string
	if err := e.h.DB.QueryRow(`SELECT rate_snapshot FROM commissions WHERE id = ?`, cid).Scan(&snap); err != nil {
		t.Fatalf("snapshot lookup: %v", err)
	}
	var r commissions.Rule
	if err := json.Unmarshal([]byte(snap), &r); err != nil {
		t.Fatalf("snapshot unmarshal: %v", err)
	}
	return r
}

// T1: below the threshold the base rate applies; crossing it switches.
func TestTiers_ThresholdCross(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedTier(t, e, "", 20000, 2500) // >= 200.00 referred -> 25%

	// First purchase: volume 0 < 20000 -> base 10%.
	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("t-1", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("below threshold: amount=%d want 1000 (base 10%%)", got)
	}
	// Second purchase: volume 10000 still < 20000 -> base again.
	cid2, err := e.e.ProcessPurchase(ctx, tierPurchase("t-2", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 1000 {
		t.Fatalf("still below: amount=%d want 1000", got)
	}
	// Third purchase: volume 20000 >= 20000 -> tier 25%.
	cid3, err := e.e.ProcessPurchase(ctx, tierPurchase("t-3", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid3); got != 2500 {
		t.Fatalf("crossed: amount=%d want 2500 (tier 25%%)", got)
	}
	// Snapshot provenance: origin, threshold, and volume at resolution.
	snap := rateSnapshot(t, e, cid3)
	if snap.Origin != "volume_tier" || snap.TierThreshold != 20000 || snap.TierVolume != 20000 {
		t.Fatalf("snapshot=%+v want volume_tier/20000/20000", snap)
	}
	if snap.RateValue != 2500 {
		t.Fatalf("snapshot rate=%d want 2500", snap.RateValue)
	}
}

// T2: the HIGHEST crossed threshold wins.
func TestTiers_HighestCrossedWins(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedTier(t, e, "", 10000, 1500)
	seedTier(t, e, "", 50000, 3000)
	seedTier(t, e, "", 100000, 4000)

	// Prime volume: one purchase at base puts gross 10000 in the bank.
	if _, err := e.e.ProcessPurchase(ctx, tierPurchase("h-0", "", 10000)); err != nil {
		t.Fatal(err)
	}
	// Volume now 10000 -> the 10000-tier (15%) is crossed.
	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("h-1", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 15 {
		t.Fatalf("amount=%d want 15 (15%% of 100)", got)
	}
	// Climb: volume 10100 < 50000 -> still 15%.
	cid2, err := e.e.ProcessPurchase(ctx, tierPurchase("h-2", "", 40000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 6000 {
		t.Fatalf("amount=%d want 6000 (still 15%% of 40000)", got)
	}
	// Volume 50100 >= 50000 -> 30%.
	cid3, err := e.e.ProcessPurchase(ctx, tierPurchase("h-3", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid3); got != 3000 {
		t.Fatalf("amount=%d want 3000 (30%% of 10000)", got)
	}
}

// T3: tiers never outrank negotiated overrides.
func TestTiers_OverridePrecedence(t *testing.T) {
	ctx := context.Background()

	// Affiliate override beats a crossed tier (threshold 0 = newcomer tier,
	// crossed from the first sale).
	t.Run("affiliate_override", func(t *testing.T) {
		e := newEnv(t)
		seedTier(t, e, "", 0, 9000) // crossed immediately
		if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
			(campaign_id, affiliate_id, event_type, rate_type, rate_value)
			VALUES (1, 1, 'purchase', 'percent', 500)`); err != nil {
			t.Fatal(err)
		}
		cid, err := e.e.ProcessPurchase(ctx, tierPurchase("ov-1", "", 10000))
		if err != nil {
			t.Fatal(err)
		}
		if got := commissionAmount(t, e, cid); got != 500 {
			t.Fatalf("amount=%d want 500 (affiliate 5%% beats tier)", got)
		}
		if snap := rateSnapshot(t, e, cid); snap.Origin != "affiliate_override" {
			t.Fatalf("origin=%s want affiliate_override", snap.Origin)
		}
	})

	// Coupon override beats a crossed tier.
	t.Run("coupon_override", func(t *testing.T) {
		e := newEnv(t)
		seedTier(t, e, "", 0, 9000)
		if _, err := e.h.DB.Exec(`INSERT INTO coupons (code, affiliate_id, rate_type, rate_value, active)
			VALUES ('tierbeater', 1, 'percent', 700, true)`); err != nil {
			t.Fatal(err)
		}
		ev := tierPurchase("ov-2", "", 10000)
		ev.CouponCode = "tierbeater"
		cid, err := e.e.ProcessPurchase(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		if got := commissionAmount(t, e, cid); got != 700 {
			t.Fatalf("amount=%d want 700 (coupon 7%% beats tier)", got)
		}
		if snap := rateSnapshot(t, e, cid); snap.Origin != "coupon" {
			t.Fatalf("origin=%s want coupon", snap.Origin)
		}
	})

	// Campaign+product economics beat the (agnostic) tier ladder.
	t.Run("campaign_product", func(t *testing.T) {
		e := newEnv(t)
		seedTier(t, e, "", 0, 9000)
		if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
			(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
			VALUES (1, NULL, 'purchase', 'percent', 800, 'pro')`); err != nil {
			t.Fatal(err)
		}
		cid, err := e.e.ProcessPurchase(ctx, tierPurchase("ov-3", "pro", 10000))
		if err != nil {
			t.Fatal(err)
		}
		if got := commissionAmount(t, e, cid); got != 800 {
			t.Fatalf("amount=%d want 800 (product 8%% beats tier)", got)
		}
		if snap := rateSnapshot(t, e, cid); snap.Origin != "campaign_product" {
			t.Fatalf("origin=%s want campaign_product", snap.Origin)
		}
		// A product with no product rule still tiers.
		cid2, err := e.e.ProcessPurchase(ctx, tierPurchase("ov-4", "basic", 10000))
		if err != nil {
			t.Fatal(err)
		}
		if got := commissionAmount(t, e, cid2); got != 9000 {
			t.Fatalf("amount=%d want 9000 (tier 90%% for unruled product)", got)
		}
	})
}

// T4: product-specific tiers outrank the product-agnostic ladder.
func TestTiers_ProductSpecific(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedTier(t, e, "", 0, 1500)           // agnostic: 15% (newcomer tier)
	seedTier(t, e, "enterprise", 0, 5000) // enterprise: 50%

	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("ps-1", "enterprise", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 5000 {
		t.Fatalf("amount=%d want 5000 (enterprise tier 50%%)", got)
	}
	// Another product falls to the agnostic ladder.
	cid2, err := e.e.ProcessPurchase(ctx, tierPurchase("ps-2", "solo", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 1500 {
		t.Fatalf("amount=%d want 1500 (agnostic tier 15%%)", got)
	}
}

// T5: the volume basis ignores refunded volume — a cancelled (refunded)
// commission must not count toward thresholds. Driven through the real
// engine (purchase -> full refund) so the money path stays true.
func TestTiers_ClawbackNotVolume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedTier(t, e, "", 10000, 2500)

	// Prime: one sale of 10000 at base 10% -> volume would be 10000.
	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("cb-0", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	// Full refund cancels the commission (and claws the money back).
	refund := purchase("cb-r", 10000)
	if err := e.e.ProcessRefund(ctx, refund, cid, 10000, 10000); err != nil {
		t.Fatal(err)
	}
	var st string
	if err := e.h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&st); err != nil || st != "cancelled" {
		t.Fatalf("parent status=%q err=%v want cancelled", st, err)
	}
	// Volume back to 0 -> next sale prices at base, tier not crossed.
	cid2, err := e.e.ProcessPurchase(ctx, tierPurchase("cb-1", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 1000 {
		t.Fatalf("amount=%d want 1000 (refunded volume must not count)", got)
	}
	if snap := rateSnapshot(t, e, cid2); snap.Origin != "campaign_default" {
		t.Fatalf("origin=%s want campaign_default", snap.Origin)
	}
}

// T6: fixed-amount tiers work (rate_type=fixed).
func TestTiers_FixedRate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO rate_tiers
		(campaign_id, product_key, threshold_minor, rate_type, rate_value)
		VALUES (1, NULL, 0, 'fixed', 250)`); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("fx-1", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 250 {
		t.Fatalf("amount=%d want 250 (fixed tier)", got)
	}
}

// T7: inactive tiers never fire.
func TestTiers_InactiveIgnored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO rate_tiers
		(campaign_id, product_key, threshold_minor, rate_type, rate_value, active)
		VALUES (1, NULL, 0, 'percent', 9000, false)`); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("ia-1", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("amount=%d want 1000 (inactive tier must not fire)", got)
	}
}

// T7b: prior-volume contract — volume is measured BEFORE the current
// event, so a threshold of 1 does NOT fire on the affiliate's first sale
// (crossing unlocks the rate on subsequent sales; see T1). Only a
// threshold of 0 (newcomer tier) fires at volume 0.
func TestTiers_ThresholdOneNotCrossedByFirstSale(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedTier(t, e, "", 1, 9000)

	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("t1-1", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("amount=%d want 1000 (threshold 1 not crossed at volume 0)", got)
	}
	if snap := rateSnapshot(t, e, cid); snap.Origin != "campaign_default" {
		t.Fatalf("origin=%s want campaign_default", snap.Origin)
	}
	// Second sale: prior volume 10000 >= 1 -> tier fires.
	cid2, err := e.e.ProcessPurchase(ctx, tierPurchase("t1-2", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 9000 {
		t.Fatalf("amount=%d want 9000 (tier crossed at volume 10000)", got)
	}
}

// T8: the fraud-provenance columns (attribution_method et al.) now persist
// from the event — the writer fraud rules were waiting for.
func TestCommission_ProvenanceColumnsPersist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev := purchase("prov-1", 10000)
	ev.CouponCode = "spring10"
	ev.CustomerExtID = "cus_777"
	ev.Country = "DE"
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	var method, coupon, custExt, country string
	var gross int64
	if err := e.h.DB.QueryRow(`SELECT attribution_method, coupon_code, customer_ext_id, country, gross_minor
		FROM commissions WHERE id = ?`, cid).Scan(&method, &coupon, &custExt, &country, &gross); err != nil {
		t.Fatal(err)
	}
	if method != "explicit" || coupon != "spring10" || custExt != "cus_777" || country != "DE" || gross != 10000 {
		t.Fatalf("provenance=(%q,%q,%q,%q,%d) want (explicit,spring10,cus_777,DE,10000)",
			method, coupon, custExt, country, gross)
	}
}
