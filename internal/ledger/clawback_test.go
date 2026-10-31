package ledger_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// P17: a refund clawback may take an affiliate payable POSITIVE (the
// affiliate owes us after a post-payout refund), while payouts keep the
// overdraft guard and can never pay more than is owed.
func TestProperty_17_ClawbackMayOverdrawPayable(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	// Credit 1000 to payable, pay it out in full: payable back to 0.
	if _, err := p.Post(ctx, "c-credit", "maturation", "", nowUTC(), []ledger.Line{
		{Account: "HOLD:USD", Amount: 1000},
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: -1000},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Post(ctx, "c-payout", "payout", "", nowUTC(), []ledger.Line{
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: 1000},
		{Account: "PAYOUT_CLEARING:USD", Amount: -1000},
	}); err != nil {
		t.Fatal(err)
	}
	claw := []ledger.Line{
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: 400},
		{Account: "EXPENSE:COMMISSION:USD", Amount: -400},
	}
	// Post keeps the guard (it cannot tell a clawback from a payout).
	if _, err := p.Post(ctx, "c-claw-plain", "clawback", "", nowUTC(), claw); err == nil {
		t.Fatal("Post accepted a payable overdraft")
	}
	// PostClawback lands it: the affiliate now owes 400.
	if _, err := p.PostClawback(ctx, "c-claw", "clawback", "", nowUTC(), claw); err != nil {
		t.Fatalf("clawback after payout refused: %v", err)
	}
	bal, err := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil {
		t.Fatal(err)
	}
	if bal != 400 {
		t.Fatalf("payable=%d want +400 (receivable: affiliate owes us)", bal)
	}
	// Replays dedupe exactly like Post.
	if _, err := p.PostClawback(ctx, "c-claw", "clawback", "", nowUTC(), claw); err != ledger.ErrDuplicate {
		t.Fatalf("clawback replay: %v (want ErrDuplicate)", err)
	}
	// Payouts keep the guard: nothing is owed, so even 1 cent is refused.
	if _, err := p.Post(ctx, "c-payout-2", "payout", "", nowUTC(), []ledger.Line{
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: 1},
		{Account: "PAYOUT_CLEARING:USD", Amount: -1},
	}); err == nil {
		t.Fatal("payout overdrew a receivable payable")
	}
	if n := journalCount(t, h.DB); n != 3 {
		t.Fatalf("journals=%d want 3 (refused posts leave nothing)", n)
	}
	if s := entrySum(t, h.DB); s != 0 {
		t.Fatalf("global sum=%d want 0", s)
	}
}
