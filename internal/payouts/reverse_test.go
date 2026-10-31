package payouts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// MO-16 (engine, both dialects): ReversePaid undoes a paid payout in one
// transaction: reversing journal, commissions paid -> approved, payout
// in the reversed state, audit row; the commissions are then paid again
// by a new batch and every ledger invariant holds.
func TestReversePaid(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	p := ledger.NewPoster(h.Ledger)
	r.WithLedger(p)
	matureOne(t, e, h, "rv-1", 1, 10000)
	matureOne(t, e, h, "rv-2", 1, 20000)
	if _, n, err := r.CreateBatch(ctx, 100, "manual"); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	var pid int64
	if err := h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := r.ReversePaid(ctx, pid, "bounced", "fin@shop"); !errors.Is(err, payouts.ErrNotPaid) {
		t.Fatalf("reverse unpaid = %v", err)
	}
	if err := r.Authorize(ctx, pid); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkPaid(ctx, pid, "WIRE-9"); err != nil {
		t.Fatal(err)
	}
	payable := func() int64 {
		b, err := p.AccountBalance(ctx, ledger.Payable(1, "USD"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if payable() != 0 {
		t.Fatalf("payable after pay = %d", payable())
	}
	if err := r.ReversePaid(ctx, pid, " ", "fin@shop"); err == nil {
		t.Fatal("reverse without a reason accepted")
	}
	if err := r.ReversePaid(ctx, pid, "bank returned the transfer", "fin@shop"); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if payable() != -3000 {
		t.Fatalf("payable after reverse = %d, want -3000", payable())
	}
	var status string
	var reversed int
	if err := h.DB.QueryRow(`SELECT status, CASE WHEN reversed_at IS NULL THEN 0 ELSE 1 END FROM payouts WHERE id = ?`, pid).Scan(&status, &reversed); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reversed != 1 {
		t.Fatalf("payout %s reversed=%d", status, reversed)
	}
	var approved, audit, journals int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status = 'approved' AND paid_at IS NULL`).Scan(&approved)
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE table_name = 'payouts' AND row_id = ? AND action = 'reverse' AND actor = 'fin@shop'`, pid).Scan(&audit)
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE idem_key = ?`, payouts.ReversalKey(pid)).Scan(&journals)
	if approved != 2 || audit != 1 || journals != 1 {
		t.Fatalf("approved %d audit %d journals %d", approved, audit, journals)
	}
	if err := r.ReversePaid(ctx, pid, "again", "fin@shop"); !errors.Is(err, payouts.ErrNotPaid) {
		t.Fatalf("second reverse = %v", err)
	}
	if sum, err := p.GlobalIntegrity(ctx); err != nil || sum != 0 {
		t.Fatalf("ledger sum %d %v", sum, err)
	}

	// Paid again by a new batch.
	if _, n, err := r.CreateBatch(ctx, 100, "manual"); err != nil || n != 1 {
		t.Fatalf("rebatch %d %v", n, err)
	}
	var pid2 int64
	_ = h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid2)
	if err := r.Approve(ctx, pid2, nil); err != nil {
		t.Fatal(err)
	}
	if payable() != 0 {
		t.Fatalf("payable after repay = %d", payable())
	}

	// A paid commission is still guarded: no direct paid -> approved.
	if _, err := h.DB.Exec(`UPDATE commissions SET status = 'approved' WHERE status = 'paid'`); err == nil {
		t.Fatal("paid -> approved allowed outside a reversal")
	}
	if _, err := h.DB.Exec(`UPDATE payouts SET status = 'failed' WHERE id = ?`, pid2); err == nil {
		t.Fatal("paid -> failed allowed without reversed_at")
	}
}

// A payout one of whose commissions was refunded after payout (clawback)
// is not reversed as a whole.
func TestReversePaidBlockedByClawback(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	r.WithLedger(ledger.NewPoster(h.Ledger))
	matureOne(t, e, h, "rb-1", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 100, "manual"); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	var pid, cid int64
	_ = h.DB.QueryRow(`SELECT id FROM payouts ORDER BY id DESC LIMIT 1`).Scan(&pid)
	_ = h.DB.QueryRow(`SELECT id FROM commissions ORDER BY id LIMIT 1`).Scan(&cid)
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ProcessRefund(ctx, mkEvent("rb-1-refund", 1, 0), cid, 10000, 10000); err != nil {
		t.Fatal(err)
	}
	if err := r.ReversePaid(ctx, pid, "bounced", "fin"); !errors.Is(err, payouts.ErrReverseBlocked) {
		t.Fatalf("reverse after clawback = %v", err)
	}
}
