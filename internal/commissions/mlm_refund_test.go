package commissions_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

// A refund takes back the MLM business volume of what it clawed: personal
// BV for the earner and group BV up the sponsor tree. A replayed refund
// takes nothing more.
func TestCommission_RefundReversesMLMBV(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mlmEng := mlm.New(e.h.DB)
	if _, err := e.h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'up@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if err := mlmEng.PlaceInSponsorTree(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	e.e.MLMBV = mlmEng.RecordBV

	cid, err := e.e.ProcessPurchase(ctx, purchase("bv-ref-p", 10000))
	if err != nil {
		t.Fatal(err)
	}
	bv := func() (personal, group int64) {
		t.Helper()
		if err := e.h.DB.QueryRow(`SELECT COALESCE(SUM(bv_personal), 0) FROM mlm_volume_daily WHERE affiliate_id = 1`).
			Scan(&personal); err != nil {
			t.Fatal(err)
		}
		if err := e.h.DB.QueryRow(`SELECT COALESCE(SUM(bv_group), 0) FROM mlm_volume_daily WHERE affiliate_id = 2`).
			Scan(&group); err != nil {
			t.Fatal(err)
		}
		return personal, group
	}
	if p, g := bv(); p != 1000 || g != 1000 {
		t.Fatalf("after purchase: personal=%d group=%d want 1000/1000", p, g)
	}

	// Refund 40%: 400 of the 1000 commission is clawed.
	if err := e.e.ProcessRefund(ctx, refundEvent("bv-ref-1"), cid, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	if p, g := bv(); p != 600 || g != 600 {
		t.Fatalf("after 40%% refund: personal=%d group=%d want 600/600", p, g)
	}
	// Replay: nothing more.
	if err := e.e.ProcessRefund(ctx, refundEvent("bv-ref-1"), cid, 4000, 10000); err == nil {
		t.Fatal("replayed refund was processed again")
	}
	if p, g := bv(); p != 600 || g != 600 {
		t.Fatalf("after replay: personal=%d group=%d want 600/600", p, g)
	}
	// Refund the rest: no volume left.
	if err := e.e.ProcessRefund(ctx, refundEvent("bv-ref-2"), cid, 6000, 10000); err != nil {
		t.Fatal(err)
	}
	if p, g := bv(); p != 0 || g != 0 {
		t.Fatalf("after full refund: personal=%d group=%d want 0/0", p, g)
	}
}
