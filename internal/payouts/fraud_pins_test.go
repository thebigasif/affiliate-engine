package payouts_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// pinsPayoutEnv: fresh DB, affiliate 1 with an approved commission worth
// 10000 cents staged into a requested payout.
func pinsPayoutEnv(t *testing.T) (*payouts.Runner, *db.Handles, int64) {
	t.Helper()
	ctx := context.Background()
	h, runner, _ := newEnv(t)
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('pinj', 'test', ?)`,
		time.Now().UTC().Format(time.RFC3339))
	jid := int64(0)
	h.DB.QueryRow(`SELECT id FROM journal_entries WHERE idem_key='pinj'`).Scan(&jid)
	if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at, matures_at, approved_at)
		VALUES (1, ?, 10000, '{}', 'approved', ?, ?, ?)`,
		jid, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed commission: %v", err)
	}
	runID, _, err := runner.CreateBatch(ctx, 1000, "manual")
	if err != nil {
		t.Fatal(err)
	}
	var payoutID int64
	h.DB.QueryRow(`SELECT p.id FROM payouts p
		JOIN payout_run_lines rl ON rl.payout_id = p.id
		WHERE rl.run_id = ? LIMIT 1`, runID).Scan(&payoutID)
	return runner, h, payoutID
}

// P1 (F5): a hold placed AFTER batch staging blocks Approve.
func TestApprove_MidCycleHoldBlocks(t *testing.T) {
	runner, h, payoutID := pinsPayoutEnv(t)
	ctx := context.Background()
	// Fraud flags one of the payout's commissions after staging.
	h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES
		((SELECT commission_id FROM payout_lines WHERE payout_id = ? LIMIT 1), 'mid-cycle flag')`, payoutID)
	err := runner.Approve(ctx, payoutID, nil)
	if err == nil {
		t.Fatal("Approve paid a commission with an open hold (F5)")
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, payoutID).Scan(&status)
	if status != "requested" {
		t.Fatalf("payout stranded in %q after hold block", status)
	}
}

// P0 (nightly 09-26/09-28 shape): settleLedger treats a duplicate settle
// key as success ONLY after verifying the journal under that key really
// moved `amount` off the affiliate's payable. The old code accepted any
// ErrDuplicate — so a reused payout id (SQLite rowid reuse after the old
// attack-1b per-iteration DELETE) or a hand-posted journal squatting the
// key marked the payout paid while the money never moved: payable kept a
// -1000 residual per paid iteration. This pins 53577e5's ErrSettleKeyConflict.
func TestApprove_SettleKeySquatted_FailsNotPaid(t *testing.T) {
	runner, h, payoutID := pinsPayoutEnv(t)
	ctx := context.Background()

	// Wire the poster like main.go does (wirePayoutLedger in the attack
	// suite mirrors this), then squat the settle key with a journal that
	// moved NOTHING off the payable.
	real := payouts.LedgerPoster
	p := ledger.NewPoster(h.Ledger)
	payouts.LedgerPoster = func(ctx context.Context, pid, aff int64, currency string, amount int64) error {
		_, err := p.Post(ctx, fmt.Sprintf("payout:%d", pid), "payout", "",
			time.Now().UTC(), payouts.SettleLines(aff, currency, amount))
		return err
	}
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = real, nil })

	squatter := func(amountMoved int64) {
		t.Helper()
		if _, err := p.Post(ctx, fmt.Sprintf("payout:%d", payoutID), "manual", "squat",
			time.Now().UTC(), []ledger.Line{
				{Account: "PAYOUT_CLEARING:USD", Amount: amountMoved},
				{Account: "EXPENSE:COMMISSION:USD", Amount: -amountMoved},
			}); err != nil {
			t.Fatalf("squat post: %v", err)
		}
	}

	// A journal under payout:<id> that moved 0 off AFFILIATE_PAYABLE:1.
	squatter(500)
	err := runner.Approve(ctx, payoutID, nil)
	if err == nil {
		t.Fatal("Approve succeeded on a squatted settle key — payout would be marked paid with money never moved")
	}
	if !errors.Is(err, payouts.ErrSettleKeyConflict) {
		t.Fatalf("err = %v, want ErrSettleKeyConflict", err)
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, payoutID).Scan(&status)
	if status != "failed" {
		t.Fatalf("payout status=%q after settle-key conflict, want failed (never quietly paid)", status)
	}
	var cStatus string
	h.DB.QueryRow(`SELECT status FROM commissions WHERE id =
		(SELECT commission_id FROM payout_lines WHERE payout_id = ? LIMIT 1)`, payoutID).Scan(&cStatus)
	if cStatus == "paid" {
		t.Fatal("commission marked paid under a squatted settle key")
	}
}

// The happy path of the duplicate rule: a journal that DID move the full
// amount (crash between settle-post and status flip) is accepted — retry
// converges instead of erroring forever.
func TestApprove_DuplicateSettleJournalVerified(t *testing.T) {
	runner, h, payoutID := pinsPayoutEnv(t)
	ctx := context.Background()

	real := payouts.LedgerPoster
	p := ledger.NewPoster(h.Ledger)
	post := func(ctx context.Context, pid, aff int64, currency string, amount int64) error {
		_, err := p.Post(ctx, fmt.Sprintf("payout:%d", pid), "payout", "",
			time.Now().UTC(), payouts.SettleLines(aff, currency, amount))
		return err
	}
	payouts.LedgerPoster = post
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = real, nil })

	// Seed the payable so the settle lines can post without overdraft:
	// credit the commission's journal first (matured approved commission
	// convention: payable carries the credit).
	var amount int64
	var aff int64
	h.DB.QueryRow(`SELECT amount, affiliate_id FROM payouts WHERE id=?`, payoutID).Scan(&amount, &aff)
	if _, err := p.Post(ctx, "seed:credit", "purchase", "", time.Now().UTC(), []ledger.Line{
		{Account: fmt.Sprintf("AFFILIATE_PAYABLE:%d:USD", aff), Amount: -amount},
		{Account: "EXPENSE:COMMISSION:USD", Amount: amount},
	}); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	// "Crash" after the settle journal landed but before the status flip.
	if err := post(ctx, payoutID, aff, "USD", amount); err != nil {
		t.Fatalf("pre-land settle: %v", err)
	}
	if err := runner.Approve(ctx, payoutID, nil); err != nil {
		t.Fatalf("Approve after crash-before-flip = %v, want nil (verified duplicate converges)", err)
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, payoutID).Scan(&status)
	if status != "paid" {
		t.Fatalf("payout status=%q, want paid", status)
	}
	bal, err := p.AccountBalance(ctx, fmt.Sprintf("AFFILIATE_PAYABLE:%d:USD", aff))
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("payable=%d after verified duplicate settle, want 0", bal)
	}
}

// P1 (F6): settleLedger failure reverts to requested — never 'processing'.
func TestApprove_SettleFailureReverts(t *testing.T) {
	runner, h, payoutID := pinsPayoutEnv(t)
	ctx := context.Background()
	prev := payouts.LedgerPoster
	payouts.LedgerPoster = func(context.Context, int64, int64, string, int64) error {
		return fmt.Errorf("injected settle failure")
	}
	payouts.Ledger = ledger.NewPoster(h.Ledger)
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = prev, nil })
	err := runner.Approve(ctx, payoutID, nil)
	if err == nil {
		t.Fatal("expected settle failure")
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, payoutID).Scan(&status)
	if status == "processing" {
		t.Fatal("payout stranded in processing (F6)")
	}
	if status != "requested" {
		t.Fatalf("want requested, got %q", status)
	}
}
