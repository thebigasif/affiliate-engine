package payouts_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// newPayout matures one commission and batches it; it returns the
// payout id.
func newPayout(t *testing.T, h *db.Handles, r *payouts.Runner, e *commissions.Engine, id string) int64 {
	t.Helper()
	matureOne(t, e, h, id, 1, 10000)
	if _, n, err := r.CreateBatch(context.Background(), 100, "paypal"); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	var pid int64
	if err := h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func payoutState(t *testing.T, h *db.Handles, pid int64) (status string, attempted bool) {
	t.Helper()
	var a int
	if err := h.DB.QueryRow(`SELECT status, CASE WHEN rail_attempted_at IS NULL THEN 0 ELSE 1 END FROM payouts WHERE id = ?`, pid).
		Scan(&status, &a); err != nil {
		t.Fatal(err)
	}
	return status, a == 1
}

// #238: a manual approval of a payout whose rail attempt may have landed
// asks the rail first, like a rail retry. Without a lookup it fails
// closed and settles nothing; a lookup that finds the transfer settles
// it with that reference; one that finds nothing lets the manual
// approval go ahead (Mark Paid takes the same path).
func TestManualApproveAfterUnknownRailAttempt(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	timeout := func(context.Context, int64, int64) (string, error) {
		return "", errors.New("read: connection reset (the credit may have landed)")
	}

	pid := newPayout(t, h, r, e, "ma-1")
	if err := r.ApproveRail(ctx, pid, timeout); err == nil {
		t.Fatal("failed rail settled")
	}
	if err := r.Approve(ctx, pid, nil); !errors.Is(err, payouts.ErrRailUnverified) {
		t.Fatalf("manual approve after an unknown attempt = %v, want ErrRailUnverified", err)
	}
	if st, _ := payoutState(t, h, pid); st != "requested" {
		t.Fatalf("status %q, want requested (nothing settled)", st)
	}

	// The lookup finds nothing: the manual approval goes ahead.
	r.RailLookup = func(context.Context, int64, int64) (string, error) { return "", nil }
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := payoutState(t, h, pid); st != "paid" {
		t.Fatalf("status %q, want paid", st)
	}

	// The lookup finds the earlier transfer: settled with it, once.
	pid = newPayout(t, h, r, e, "ma-2")
	r.RailLookup = nil
	if err := r.ApproveRail(ctx, pid, timeout); err == nil {
		t.Fatal("failed rail settled")
	}
	r.RailLookup = func(context.Context, int64, int64) (string, error) { return "CREDIT-LANDED", nil }
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}
	var st, ref string
	_ = h.DB.QueryRow(`SELECT status, provider_ref FROM payouts WHERE id = ?`, pid).Scan(&st, &ref)
	if st != "paid" || ref != "CREDIT-LANDED" {
		t.Fatalf("status %q ref %q, want paid CREDIT-LANDED", st, ref)
	}
	if err := r.Approve(ctx, pid, nil); !errors.Is(err, payouts.ErrAlreadyPaid) {
		t.Fatalf("second manual approve = %v, want ErrAlreadyPaid", err)
	}
}

// #238: a rail that refuses before sending (ErrNotSent) leaves no "may
// have been paid" marker, so the payout can be paid another way. A
// marker an earlier attempt set stays.
func TestNotSentClearsOnlyItsOwnAttempt(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	refused := func(context.Context, int64, int64) (string, error) {
		return "", fmt.Errorf("store credit: %w: missing scope", payouts.ErrNotSent)
	}

	pid := newPayout(t, h, r, e, "ns-1")
	if err := r.ApproveRail(ctx, pid, refused); !errors.Is(err, payouts.ErrNotSent) {
		t.Fatalf("refused rail = %v, want ErrNotSent", err)
	}
	if st, attempted := payoutState(t, h, pid); st != "requested" || attempted {
		t.Fatalf("status %q attempted %v, want requested and no attempt", st, attempted)
	}
	// Nothing to verify: a manual approval needs no lookup.
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatalf("manual approve after a refusal = %v", err)
	}

	// An earlier attempt with an unknown outcome is not erased by a
	// later refusal.
	pid = newPayout(t, h, r, e, "ns-2")
	timeout := func(context.Context, int64, int64) (string, error) { return "", errors.New("timeout") }
	if err := r.ApproveRail(ctx, pid, timeout); err == nil {
		t.Fatal("failed rail settled")
	}
	r.RailLookup = func(context.Context, int64, int64) (string, error) { return "", nil }
	if err := r.ApproveRail(ctx, pid, refused); !errors.Is(err, payouts.ErrNotSent) {
		t.Fatalf("refused rail = %v, want ErrNotSent", err)
	}
	if _, attempted := payoutState(t, h, pid); !attempted {
		t.Fatal("a later refusal erased an earlier attempt's marker")
	}
}
