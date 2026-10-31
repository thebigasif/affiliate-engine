package commissions_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// FR-12 per-product/plan rates: rule-resolution tier precedence.
//
// coupon > affiliate+product > affiliate (product-agnostic) >
// campaign+product > campaign default.
//
// Every test runs through ProcessPurchase so the snapshotted amount on the
// commissions row is the observable, money-true outcome.

func productPurchase(id, key string, amount int64) commissions.Event {
	ev := purchase(id, amount)
	ev.ProductKey = key
	return ev
}

func commissionAmount(t *testing.T, e *env, cid int64) int64 {
	t.Helper()
	var amount int64
	if err := e.h.DB.QueryRow(`SELECT amount FROM commissions WHERE id = ?`, cid).
		Scan(&amount); err != nil {
		t.Fatalf("commission lookup: %v", err)
	}
	return amount
}

// P1: campaign+product rule outranks the product-agnostic campaign default.
func TestProductRates_CampaignProductBeatsDefault(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// seed() installed the 10% campaign default (1000 bp).
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, NULL, 'purchase', 'percent', 3000, 'price_pro')`); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, productPurchase("pp-1", "price_pro", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 3000 {
		t.Fatalf("amount=%d want 3000 (campaign+product 30%%)", got)
	}
	// A different product still falls back to the default 10%.
	cid2, err := e.e.ProcessPurchase(ctx, productPurchase("pp-2", "price_basic", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 1000 {
		t.Fatalf("other product amount=%d want 1000 (default 10%%)", got)
	}
}

// P2: affiliate+product outranks campaign+product AND campaign default.
func TestProductRates_AffiliateProductBeatsAllCampaignTiers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.h.DB.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, NULL, 'purchase', 'percent', 3000, 'price_pro')`) // campaign+product
	exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, 1, 'purchase', 'percent', 5000, 'price_pro')`) // affiliate+product 50%
	cid, err := e.e.ProcessPurchase(ctx, productPurchase("pp-3", "price_pro", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 5000 {
		t.Fatalf("amount=%d want 5000 (affiliate+product 50%%)", got)
	}
}

// P3: product-agnostic affiliate override outranks campaign+product —
// specificity is within a tier's owner, not across owners.
func TestProductRates_AffiliateAgnosticBeatsCampaignProduct(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, NULL, 'purchase', 'percent', 3000, 'price_pro')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (1, 1, 'purchase', 'fixed', 2500)`); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, productPurchase("pp-4", "price_pro", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 2500 {
		t.Fatalf("amount=%d want 2500 (affiliate override fixed)", got)
	}
}

// P4: coupon still outranks every product tier.
func TestProductRates_CouponBeatsProductTiers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO coupons (code, affiliate_id, rate_type, rate_value)
		VALUES ('SUMMER', 1, 'fixed', 500)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, NULL, 'purchase', 'percent', 3000, 'price_pro')`); err != nil {
		t.Fatal(err)
	}
	ev := productPurchase("pp-5", "price_pro", 10000)
	ev.CouponCode = "SUMMER"
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 500 {
		t.Fatalf("amount=%d want 500 (coupon fixed beats product tiers)", got)
	}
}

// P5: priority disambiguates within one product tier.
func TestProductRates_PriorityWithinTier(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, r := range []struct {
		priority int
		value    int64
	}{
		{priority: 1, value: 2000},
		{priority: 5, value: 4000},
		{priority: 3, value: 3100},
	} {
		if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
			(campaign_id, affiliate_id, event_type, rate_type, rate_value, priority, product_key)
			VALUES (1, NULL, 'purchase', 'percent', ?, ?, 'price_pro')`, r.value, r.priority); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := e.e.ProcessPurchase(ctx, productPurchase("pp-6", "price_pro", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 4000 {
		t.Fatalf("amount=%d want 4000 (highest priority in product tier)", got)
	}
}

// P6: an event with no product key never matches a product-specific rule —
// it resolves through the product-agnostic tiers only.
func TestProductRates_NoKeyNeverMatchesSpecificRule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Replace the default: deactivate it, leaving ONLY a product rule.
	if _, err := e.h.DB.Exec(`UPDATE commission_rules SET active = FALSE
		WHERE campaign_id = 1 AND affiliate_id IS NULL AND product_key IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, NULL, 'purchase', 'percent', 9000, 'price_pro')`); err != nil {
		t.Fatal(err)
	}
	// No-key event: no rule matches at all -> explicit error, no commission.
	if _, err := e.e.ProcessPurchase(ctx, productPurchase("pp-7", "", 10000)); err == nil {
		t.Fatal("keyless event unexpectedly credited against a product-specific-only ruleset")
	}
	// Keyed event still resolves.
	cid, err := e.e.ProcessPurchase(ctx, productPurchase("pp-8", "price_pro", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 9000 {
		t.Fatalf("amount=%d want 9000 (product rule 90%%)", got)
	}
}

// P7: renewal events share the product-aware resolution (per-billing-event
// recompute honors per-plan rates — FR-12 recurring clause).
func TestProductRates_RenewalUsesProductRule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules
		(campaign_id, affiliate_id, event_type, rate_type, rate_value, product_key)
		VALUES (1, NULL, 'purchase', 'percent', 2500, 'price_pro')`); err != nil {
		t.Fatal(err)
	}
	ev := productPurchase("pp-9", "price_pro", 10000)
	ev.EventType = "renewal"
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 2500 {
		t.Fatalf("renewal amount=%d want 2500 (product rule 25%%)", got)
	}
}
