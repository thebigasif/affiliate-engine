package attribution_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
)

// A8: frozen/removed affiliates never earn — every rung that lands on a
// non-active affiliate resolves unattributed instead of crediting it.
func TestLadder_InactiveAffiliateUnattributed(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"frozen", "frozen"},
		{"removed", "removed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, r, s := newEnv(t)
			ctx := context.Background()
			// Affiliate 1 owns coupon SAVE10 and click clk_i; affiliate 2
			// owns customer binding cus_9.
			exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
			         VALUES ('clk_i', ?, 20260915, 202609, 1, 1, ?)`,
				time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
				[]byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2, 3, 4, 5, 6})
			exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
			         VALUES ('clk_i', 1, 1, 'token', CURRENT_TIMESTAMP, '`+dbtest.At(1*24*time.Hour)+`')`)
			exec(h, `UPDATE affiliates SET status = ? WHERE id IN (1, 2)`, c.status)

			for _, q := range []attribution.Query{
				{CouponCode: "SAVE10", CampaignID: 1, Now: time.Now()},
				{CustomerExtID: "cus_9", CampaignID: 1, Now: time.Now()},
				{Token: validToken(t, s, "clk_i", 1), CampaignID: 1, Now: time.Now()},
				{IPHash: []byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2, 3, 4, 5, 6}, CampaignID: 1, Now: time.Now()},
			} {
				res, err := r.Resolve(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if res.Method != "" || res.AffiliateID != 0 {
					t.Fatalf("%s affiliate earned via %+v (query %+v)", c.status, res, q)
				}
			}
		})
	}
}

// A9: a frozen coupon owner does not hand the sale to a weaker rung (a
// different affiliate's token) — the ladder stops unattributed.
func TestLadder_FrozenCouponDoesNotFallThrough(t *testing.T) {
	h, r, s := newEnv(t)
	exec(h, `INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
	         VALUES ('clk_f', CURRENT_TIMESTAMP, 20260915, 202609, 2, 1)`)
	exec(h, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
	         VALUES ('clk_f', 2, 1, 'token', CURRENT_TIMESTAMP, '`+dbtest.At(1*24*time.Hour)+`')`)
	exec(h, `UPDATE affiliates SET status = 'frozen' WHERE id = 1`)
	res, err := r.Resolve(context.Background(), attribution.Query{
		CouponCode: "SAVE10", Token: validToken(t, s, "clk_f", 2), CampaignID: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "" {
		t.Fatalf("frozen coupon fell through to %+v", res)
	}
	// Re-activated: the coupon earns again.
	exec(h, `UPDATE affiliates SET status = 'active' WHERE id = 1`)
	res, err = r.Resolve(context.Background(), attribution.Query{CouponCode: "SAVE10", CampaignID: 1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "coupon" || res.AffiliateID != 1 {
		t.Fatalf("active coupon owner not attributed: %+v", res)
	}
}
