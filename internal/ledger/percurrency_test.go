package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// A journal moves one currency: Post refuses lines in two currencies, and
// so does the database guard for a hand-written journal.
func TestPerCurrency_MixedJournalRefused(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	_, err := p.Post(ctx, "mix", "conversion", "", nowUTC(), []ledger.Line{
		{Account: ledger.Expense("USD"), Amount: 100},
		{Account: ledger.Hold("JPY"), Amount: -100},
	})
	if !errors.Is(err, ledger.ErrMixedCurrency) {
		t.Fatalf("USD/JPY journal: %v want ErrMixedCurrency", err)
	}
	if n := journalCount(t, h.DB); n != 0 {
		t.Fatalf("journals=%d after a refused post", n)
	}

	// The database guard, below Go.
	if _, err := p.Post(ctx, "jpy-seed", "conversion", "", nowUTC(), []ledger.Line{
		{Account: ledger.Expense("JPY"), Amount: 1},
		{Account: ledger.Hold("JPY"), Amount: -1},
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := h.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var jid int64
	if err := tx.QueryRow(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('raw-mix', 'x', '2026-01-01T00:00:00Z') RETURNING id`).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO ledger_entries (journal_id, account_id, amount)
		SELECT ?, id, 5 FROM ledger_accounts WHERE code = 'EXPENSE:COMMISSION:USD'`, jid); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO ledger_entries (journal_id, account_id, amount)
		SELECT ?, id, -5 FROM ledger_accounts WHERE code = 'HOLD:JPY'`, jid)
	if err == nil || !strings.Contains(err.Error(), "journal_mixed_currency") {
		t.Fatalf("raw USD/JPY journal: %v want journal_mixed_currency", err)
	}
}

// Per-currency accounts are created on first use; a payable only for an
// affiliate that exists, and never on a legacy currency-less account.
func TestPerCurrency_AccountsOnFirstUse(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	if _, err := p.Post(ctx, "eur-credit", "maturation", "", nowUTC(), []ledger.Line{
		{Account: ledger.Hold("EUR"), Amount: 700},
		{Account: ledger.Payable(1, "EUR"), Amount: -700},
	}); err != nil {
		t.Fatalf("first EUR post: %v", err)
	}
	var cur string
	if err := h.DB.QueryRow(`SELECT currency FROM ledger_accounts WHERE code = 'AFFILIATE_PAYABLE:1:EUR'`).Scan(&cur); err != nil || cur != "EUR" {
		t.Fatalf("payable account currency=%q err=%v", cur, err)
	}
	if _, err := p.Post(ctx, "ghost", "maturation", "", nowUTC(), []ledger.Line{
		{Account: ledger.Hold("EUR"), Amount: 1},
		{Account: ledger.Payable(999, "EUR"), Amount: -1},
	}); err == nil {
		t.Fatal("posted to the payable of an affiliate that does not exist")
	}
	if _, err := h.DB.Exec(`INSERT INTO ledger_accounts (code, kind) VALUES ('HOLD', 'liability')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Post(ctx, "legacy", "conversion", "", nowUTC(), []ledger.Line{
		{Account: "HOLD", Amount: -1},
		{Account: ledger.Expense("USD"), Amount: 1},
	}); err == nil || !strings.Contains(err.Error(), "no currency") {
		t.Fatalf("post to a legacy account: %v", err)
	}
}

// The overdraft guard is per currency: a yen receivable does not stop a
// dollar payout, and dollars owed do not cover a yen payout.
func TestPerCurrency_OverdraftPerCurrency(t *testing.T) {
	_, p := newTestDB(t)
	ctx := context.Background()
	post := func(key string, lines ...ledger.Line) error {
		_, err := p.Post(ctx, key, "x", "", nowUTC(), lines)
		return err
	}
	if err := post("usd-owed", ledger.Line{Account: ledger.Hold("USD"), Amount: 1000},
		ledger.Line{Account: ledger.Payable(1, "USD"), Amount: -1000}); err != nil {
		t.Fatal(err)
	}
	// A yen receivable (clawback after payout).
	if _, err := p.PostClawback(ctx, "jpy-claw", "clawback", "", nowUTC(), []ledger.Line{
		{Account: ledger.Payable(1, "JPY"), Amount: 5000},
		{Account: ledger.Expense("JPY"), Amount: -5000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := post("jpy-pay", ledger.Line{Account: ledger.Payable(1, "JPY"), Amount: 1},
		ledger.Line{Account: ledger.Clearing("JPY"), Amount: -1}); err == nil {
		t.Fatal("yen payout drew on dollars owed")
	}
	if err := post("usd-pay", ledger.Line{Account: ledger.Payable(1, "USD"), Amount: 1000},
		ledger.Line{Account: ledger.Clearing("USD"), Amount: -1000}); err != nil {
		t.Fatalf("dollar payout blocked by a yen receivable: %v", err)
	}
}
