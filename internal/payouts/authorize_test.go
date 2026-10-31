package payouts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// MO-20 (engine, both dialects): Authorize moves a staged payout to
// approved without paying; a commission that leaves 'approved' before
// approval releases the payout instead (ErrRestaged), its survivors
// back in the pool; Release of an unpaid payout returns its commissions.
func TestAuthorizeAndRelease(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "az-1", 1, 10000)
	matureOne(t, e, h, "az-2", 1, 20000)
	if _, n, err := r.CreateBatch(ctx, 100, "manual"); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	var pid int64
	if err := h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := r.Authorize(ctx, pid); err != nil {
		t.Fatal(err)
	}
	if err := r.Authorize(ctx, pid); err != nil {
		t.Fatalf("second authorize: %v", err)
	}
	var status string
	_ = h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pid).Scan(&status)
	if status != "approved" {
		t.Fatalf("status %s", status)
	}
	var paid int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status = 'paid'`).Scan(&paid)
	if paid != 0 {
		t.Fatal("authorize paid commissions")
	}
	if err := r.Release(ctx, pid, "declined"); err != nil {
		t.Fatalf("release: %v", err)
	}
	var lines int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines`).Scan(&lines)
	if lines != 0 {
		t.Fatalf("lines after release = %d", lines)
	}
	if err := r.MarkPaid(ctx, pid, "x"); err == nil {
		t.Fatal("released payout marked paid")
	}

	// Restage at approval: a member cancelled after staging.
	if _, n, err := r.CreateBatch(ctx, 100, "manual"); err != nil || n != 1 {
		t.Fatalf("rebatch %d %v", n, err)
	}
	if err := h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE commissions SET status = 'cancelled' WHERE id = (SELECT MIN(id) FROM commissions)`); err != nil {
		t.Fatal(err)
	}
	if err := r.Authorize(ctx, pid); !errors.Is(err, payouts.ErrRestaged) {
		t.Fatalf("authorize after a member left = %v, want ErrRestaged", err)
	}
	planned, err := r.Preview(ctx, 100)
	if err != nil || len(planned) != 1 || planned[0].Cents != 2000 {
		t.Fatalf("survivor not back in the pool: %+v %v", planned, err)
	}
}
