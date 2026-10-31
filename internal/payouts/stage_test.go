package payouts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// MO-12 (engine, both dialects): Stage claims whole commissions within
// the maximum, oldest first, into a requested payout; claimed money
// leaves Payable and every batch; a stage that rolls back claims
// nothing.
func TestStageClaimsWholeCommissions(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "st-1", 1, 30000) // 3000
	matureOne(t, e, h, "st-2", 1, 20000) // 2000
	matureOne(t, e, h, "st-3", 1, 10000) // 1000
	if p, err := r.Payable(ctx, 1, "usd"); err != nil || p != 6000 {
		t.Fatalf("payable %d %v", p, err)
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stage(ctx, tx, 1, "USD", 4500, "manual"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if p, _ := r.Payable(ctx, 1, "USD"); p != 6000 {
		t.Fatalf("rolled-back stage claimed: payable %d", p)
	}

	tx, err = h.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Stage(ctx, tx, 1, "USD", 4500, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 3000 fits, 2000 would exceed 4500, 1000 fits: 4000.
	if st.Amount != 4000 {
		t.Fatalf("staged %d, want 4000", st.Amount)
	}
	if p, _ := r.Payable(ctx, 1, "USD"); p != 2000 {
		t.Fatalf("payable after stage %d, want 2000", p)
	}
	planned, err := r.Preview(ctx, 0)
	if err != nil || len(planned) != 1 || planned[0].Cents != 2000 {
		t.Fatalf("batch sees %+v %v", planned, err)
	}

	tx, _ = h.DB.BeginTx(ctx, nil)
	defer func() { _ = tx.Rollback() }()
	if _, err := r.Stage(ctx, tx, 1, "USD", 1500, "manual"); !errors.Is(err, payouts.ErrNothingPayable) {
		t.Fatalf("nothing fits = %v", err)
	}
}
