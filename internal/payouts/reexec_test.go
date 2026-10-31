package payouts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// MO-21: a payout whose rail was attempted is never sent again blindly.
// Without RailLookup the re-approve fails closed (ErrRailUnverified,
// attention set, no second send); a lookup that finds the transfer
// settles it with the provider's reference and sends nothing; only a
// lookup that finds nothing re-executes. The executor's reference is
// stored on the payout.
func TestRailAttemptedIsLookedUpBeforeResend(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "rx-1", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 100, "paypal"); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	var pid int64
	if err := h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	sends := 0
	timeout := func(context.Context, int64, int64) (string, error) {
		sends++
		return "", errors.New("read: connection reset (the transfer may have landed)")
	}
	if err := r.ApproveRail(ctx, pid, timeout); err == nil {
		t.Fatal("failed rail settled")
	}
	ok := func(context.Context, int64, int64) (string, error) {
		sends++
		return "BATCH-NEW", nil
	}
	// No lookup: fail closed, nothing sent.
	if err := r.ApproveRail(ctx, pid, ok); !errors.Is(err, payouts.ErrRailUnverified) {
		t.Fatalf("re-approve without lookup = %v, want ErrRailUnverified", err)
	}
	if sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
	var status, attention string
	_ = h.DB.QueryRow(`SELECT status, COALESCE(attention,'') FROM payouts WHERE id = ?`, pid).Scan(&status, &attention)
	if status != "requested" || attention == "" {
		t.Fatalf("status %q attention %q", status, attention)
	}
	// Lookup finds nothing: re-executes once, stores the reference.
	r.RailLookup = func(context.Context, int64, int64) (string, error) { return "", nil }
	if err := r.ApproveRail(ctx, pid, ok); err != nil {
		t.Fatal(err)
	}
	if sends != 2 {
		t.Fatalf("sends = %d, want 2", sends)
	}
	var ref string
	_ = h.DB.QueryRow(`SELECT status, provider_ref FROM payouts WHERE id = ?`, pid).Scan(&status, &ref)
	if status != "paid" || ref != "BATCH-NEW" {
		t.Fatalf("status %q ref %q", status, ref)
	}

	// Lookup finds the earlier transfer: settled with it, nothing sent.
	matureOne(t, e, h, "rx-2", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 100, "paypal"); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	if err := h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := r.ApproveRail(ctx, pid, timeout); err == nil {
		t.Fatal("failed rail settled")
	}
	r.RailLookup = func(_ context.Context, id, amount int64) (string, error) {
		if id != pid || amount != 1000 {
			t.Errorf("lookup(%d, %d)", id, amount)
		}
		return "BATCH-LANDED", nil
	}
	sends = 0
	if err := r.ApproveRail(ctx, pid, ok); err != nil {
		t.Fatal(err)
	}
	_ = h.DB.QueryRow(`SELECT status, provider_ref FROM payouts WHERE id = ?`, pid).Scan(&status, &ref)
	if sends != 0 || status != "paid" || ref != "BATCH-LANDED" {
		t.Fatalf("sends %d status %q ref %q", sends, status, ref)
	}
}
