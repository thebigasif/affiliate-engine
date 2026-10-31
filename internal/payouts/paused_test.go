package payouts_test

import (
	"context"
	"testing"
)

// A paused (frozen) partner is not paid: their commissions stay out of new
// payouts, and a payout staged before the pause can't be approved until
// they're resumed. Nothing is lost: resuming makes it all payable again.
func TestPausedAffiliate_NotPaid(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	matureOne(t, e, h, "pz-a", 1, 10000)
	status := func(s string) {
		t.Helper()
		if _, err := h.DB.Exec(`UPDATE affiliates SET status = ? WHERE id = 1`, s); err != nil {
			t.Fatal(err)
		}
	}

	status("frozen")
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 0 {
		t.Fatalf("paused partner batched: n=%d err=%v", n, err)
	}

	status("active")
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("resumed partner not batched: n=%d err=%v", n, err)
	}
	pid := payoutOf(t, h, 1)

	status("frozen")
	if err := r.Approve(ctx, pid, nil); err == nil {
		t.Fatal("approved a payout to a paused partner")
	}
	var st string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pid).Scan(&st)
	if st != "requested" {
		t.Fatalf("payout status=%s want requested (kept, not restaged)", st)
	}

	status("active")
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatalf("approve after resume: %v", err)
	}
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pid).Scan(&st)
	if st != "paid" {
		t.Fatalf("payout status=%s want paid", st)
	}
}
