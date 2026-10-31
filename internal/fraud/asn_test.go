package fraud_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// FA3: 80%+ datacenter clicks over 7d flags (once the affiliate has real
// conversions); mixed traffic doesn't.
func TestFraud_ASNReputation(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	day := time.Now().UTC().Format("20060102")
	// Affiliate 1: 25 dc / 30 total (83%).
	h.DB.Exec(`INSERT INTO dc_click_daily (day, affiliate_id, dc_clicks, total_clicks)
		VALUES (?, 1, 25, 30)`, day)
	// Affiliate 2: 5 dc / 40 total (12%).
	h.DB.Exec(`INSERT INTO dc_click_daily (day, affiliate_id, dc_clicks, total_clicks)
		VALUES (?, 2, 5, 40)`, day)
	// Click-share rules need attributed conversions before they enforce.
	seedCommissions(t, h, 1, 12, 3)

	r := fraud.ASNReputation{DB: h.DB}
	vs, err := r.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Action != fraud.ActionHold {
		t.Fatalf("dc-heavy not flagged: %+v", vs)
	}
	vs2, _ := r.Evaluate(ctx, fraud.Query{AffiliateID: 2, Now: time.Now()})
	if len(vs2) != 0 {
		t.Fatalf("mixed traffic flagged: %+v", vs2)
	}
	// Small sample: no verdict.
	h.DB.Exec(`INSERT INTO dc_click_daily (day, affiliate_id, dc_clicks, total_clicks)
		VALUES (?, 3, 10, 10)`, day)
	vs3, _ := r.Evaluate(ctx, fraud.Query{AffiliateID: 3, Now: time.Now()})
	if len(vs3) != 0 {
		t.Fatalf("min-sample violated: %+v", vs3)
	}
	_ = db.Open
}
