package commissions_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// The engine's own coupon/customer fallback (an adapter passed
// AffiliateID=0) must not credit a frozen or removed affiliate: the
// attribution ladder already said unattributed for them.
func TestFallback_InactiveAffiliateNotCredited(t *testing.T) {
	for _, st := range []string{"frozen", "removed"} {
		t.Run(st, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			if _, err := e.h.DB.Exec(`INSERT INTO coupons (code, affiliate_id, rate_type, rate_value)
				VALUES ('FRZ', 1, 'fixed', 500)`); err != nil {
				t.Fatal(err)
			}
			if _, err := e.h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method)
				VALUES (1, 'cus_frz', 1, 'token')`); err != nil {
				t.Fatal(err)
			}
			if _, err := e.h.DB.Exec(`UPDATE affiliates SET status = ? WHERE id = 1`, st); err != nil {
				t.Fatal(err)
			}
			coupon := purchase("evt-frz-coupon", 10000)
			coupon.AffiliateID = 0
			coupon.CouponCode = "FRZ"
			if _, err := e.e.ProcessPurchase(ctx, coupon); err != commissions.ErrNoAttribution {
				t.Fatalf("coupon: want ErrNoAttribution, got %v", err)
			}
			cust := purchase("evt-frz-cust", 10000)
			cust.AffiliateID = 0
			cust.CustomerExtID = "cus_frz"
			if _, err := e.e.ProcessPurchase(ctx, cust); err != commissions.ErrNoAttribution {
				t.Fatalf("customer: want ErrNoAttribution, got %v", err)
			}
			var n int
			if err := e.h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("%d commissions credited to a %s affiliate", n, st)
			}
		})
	}
}

// A customer binding resolved by the engine's fallback credits in the
// program the customer was bound under, like the attribution ladder.
func TestFallback_CustomerCreditsInBoundProgram(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (2, NULL, 'purchase', 'percent', 3000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method, campaign_id)
		VALUES (1, 'cus_p2', 1, 'token', 2)`); err != nil {
		t.Fatal(err)
	}
	ev := purchase("evt-p2", 10000)
	ev.AffiliateID = 0
	ev.CustomerExtID = "cus_p2"
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	var camp, amount int64
	if err := e.h.DB.QueryRow(`SELECT campaign_id, amount FROM commissions WHERE id = ?`, cid).Scan(&camp, &amount); err != nil {
		t.Fatal(err)
	}
	if camp != 2 || amount != 3000 {
		t.Fatalf("campaign=%d amount=%d, want program 2 at 30%% (3000)", camp, amount)
	}
}

// daily_agg (the dashboard totals) records the RESOLVED affiliate, not
// the event's AffiliateID, which is 0 for coupon/customer attribution.
func TestDailyAgg_CouponAttributionRecordsResolvedAffiliate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO coupons (code, affiliate_id, rate_type, rate_value)
		VALUES ('DAY', 1, 'fixed', 500)`); err != nil {
		t.Fatal(err)
	}
	ev := purchase("evt-day-coupon", 10000)
	ev.AffiliateID = 0
	ev.CouponCode = "DAY"
	if _, err := e.e.ProcessPurchase(ctx, ev); err != nil {
		t.Fatal(err)
	}
	var zero, one int64
	if err := e.h.DB.QueryRow(`SELECT COALESCE(SUM(commission_cents),0) FROM daily_agg WHERE affiliate_id = 0`).Scan(&zero); err != nil {
		t.Fatal(err)
	}
	if err := e.h.DB.QueryRow(`SELECT COALESCE(SUM(commission_cents),0) FROM daily_agg WHERE affiliate_id = 1`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if zero != 0 || one != 500 {
		t.Fatalf("daily_agg affiliate 0=%d, affiliate 1=%d; want 0 and 500", zero, one)
	}
}
