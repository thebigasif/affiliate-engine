package payouts_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// fakeRail is a money rail whose transfers always land at the provider;
// timeout makes the client see an error anyway (the response was lost).
type fakeRail struct {
	timeout   bool
	transfers map[int64]string // payout id -> transfer id
	sends     int
}

func newFakeRail() *fakeRail { return &fakeRail{transfers: map[int64]string{}} }

func (f *fakeRail) execute(payoutID, _ int64) error {
	if _, ok := f.transfers[payoutID]; !ok {
		f.sends++
		f.transfers[payoutID] = fmt.Sprintf("tr_%d", f.sends)
	}
	if f.timeout {
		return errors.New("stripe: network: context deadline exceeded")
	}
	return nil
}

func (f *fakeRail) lookup(_ context.Context, payoutID, _ int64) (string, error) {
	return f.transfers[payoutID], nil
}

// A transfer that timed out client-side but landed at the provider, then a
// clawback that makes the gate restage the payout: the survivors must not
// be re-batched into a second payout and a second real transfer.
func TestApprove_TimedOutTransferNeverRestaged(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	rail := newFakeRail()
	r.RailLookup = rail.lookup

	c1 := creditMature(t, e, h, mkEvent("rt-1", 1, 10000)) // 1000
	if _, n, err := r.CreateBatch(ctx, 0, "stripe_connect"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	pid, _ := onlyPayout(t, h, "requested")

	rail.timeout = true
	if err := r.Approve(ctx, pid, rail.execute); err == nil {
		t.Fatal("timed-out transfer reported success")
	}
	rail.timeout = false
	if rail.sends != 1 {
		t.Fatalf("sends=%d want 1", rail.sends)
	}

	if err := e.ProcessRefund(ctx, refundOf("rt-ref"), c1, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	err := r.Approve(ctx, pid, rail.execute)
	if errors.Is(err, payouts.ErrRestaged) {
		t.Fatalf("payout whose transfer landed was restaged: %v", err)
	}
	var lines int
	var status string
	var ref, attention *string
	if err := h.DB.QueryRow(`SELECT status, provider_ref, attention FROM payouts WHERE id = ?`, pid).
		Scan(&status, &ref, &attention); err != nil {
		t.Fatal(err)
	}
	h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines WHERE payout_id = ?`, pid).Scan(&lines)
	if lines == 0 {
		t.Fatal("lines released: the commissions would be paid again")
	}
	if status != "paid" && status != "failed" {
		t.Fatalf("status=%s want paid (or failed for the operator), never back in the pool", status)
	}
	if ref == nil || *ref != "tr_1" {
		t.Fatalf("provider_ref=%v want tr_1", ref)
	}
	if attention == nil || *attention == "" {
		t.Fatal("no attention note: the operator must reconcile the clawback")
	}

	if _, n, err := r.CreateBatch(ctx, 0, "stripe_connect"); err != nil || n != 0 {
		t.Fatalf("re-batch staged %d payouts (err=%v): the landed transfer would be paid again", n, err)
	}
	if rail.sends != 1 {
		t.Fatalf("sends=%d want 1", rail.sends)
	}
}

// Without a way to ask the rail, a payout whose rail was attempted is
// flagged, never restaged.
func TestApprove_RailAttemptedWithoutLookupFlagged(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	rail := newFakeRail()
	rail.timeout = true

	c1 := creditMature(t, e, h, mkEvent("rn-1", 1, 10000))
	if _, n, err := r.CreateBatch(ctx, 0, "stripe_connect"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	pid, _ := onlyPayout(t, h, "requested")
	if err := r.Approve(ctx, pid, rail.execute); err == nil {
		t.Fatal("timed-out transfer reported success")
	}
	if err := e.ProcessRefund(ctx, refundOf("rn-ref"), c1, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	err := r.Approve(ctx, pid, rail.execute)
	if !errors.Is(err, payouts.ErrRailUnverified) {
		t.Fatalf("approve: %v (want ErrRailUnverified)", err)
	}
	var status string
	var attention *string
	h.DB.QueryRow(`SELECT status, attention FROM payouts WHERE id = ?`, pid).Scan(&status, &attention)
	if status != "requested" || attention == nil {
		t.Fatalf("status=%s attention=%v, want requested with attention", status, attention)
	}
	var lines int
	h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines WHERE payout_id = ?`, pid).Scan(&lines)
	if lines == 0 {
		t.Fatal("lines released")
	}
}

// A payout whose rail was attempted but whose transfer never landed still
// restages (the rail says there is nothing to adopt).
func TestApprove_RailAttemptedNothingLandedRestages(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	r.RailLookup = func(context.Context, int64, int64) (string, error) { return "", nil }

	c1 := creditMature(t, e, h, mkEvent("rl-1", 1, 10000))
	if _, n, err := r.CreateBatch(ctx, 0, "stripe_connect"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	pid, _ := onlyPayout(t, h, "requested")
	failing := func(int64, int64) error { return errors.New("stripe: 500") }
	if err := r.Approve(ctx, pid, failing); err == nil {
		t.Fatal("failed transfer reported success")
	}
	if err := e.ProcessRefund(ctx, refundOf("rl-ref"), c1, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, pid, failing); !errors.Is(err, payouts.ErrRestaged) {
		t.Fatalf("approve: %v (want ErrRestaged)", err)
	}
}
