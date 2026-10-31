package migrations_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// testdata/legacy_mixed_currency.sql is the money tables of a database
// written by the release before per-currency accounts (98a283a), running
// its real money paths on SQLite:
//
//	affiliate 1: $100.00 (USD sale) and ¥20,000 (JPY sale) commissions,
//	  both matured into the ONE payable AFFILIATE_PAYABLE:1; the ¥20,000
//	  payout paid, the $100.00 payout staged; then a full yen refund after
//	  payout (a ¥20,000 receivable) and a $10.00 partial dollar refund.
//	  Old balance of AFFILIATE_PAYABLE:1: +11000, a meaningless mix.
//	affiliate 2: $50.00 paid out, $30.00 upheld as fraud, $20.00 pending.
//
// The fixture's ledger accounts have no currency (the old shape): loading
// it onto a migrated schema reproduces an un-upgraded install's ledger.
func loadLegacy(t *testing.T, h *db.Handles, extra ...string) {
	t.Helper()
	b, err := os.ReadFile("testdata/legacy_mixed_currency.sql")
	if err != nil {
		t.Fatal(err)
	}
	var stmts []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "INSERT INTO ") {
			stmts = append(stmts, line)
		}
	}
	for _, s := range append(stmts, extra...) {
		if _, err := h.DB.Exec(s); err != nil {
			t.Fatalf("fixture: %v\n%s", err, s)
		}
	}
	syncSequences(t, h)
}

// syncSequences moves Postgres id sequences past rows inserted with
// explicit ids (the migration creates accounts after them).
func syncSequences(t *testing.T, h *db.Handles) {
	t.Helper()
	if dbtest.IsPostgres() {
		for _, tbl := range []string{
			"affiliates", "commission_rules", "ledger_accounts", "journal_entries",
			"ledger_entries", "raw_events", "commissions", "payout_runs", "payouts",
		} {
			if _, err := h.DB.Exec(`SELECT setval(pg_get_serial_sequence('` + tbl + `', 'id'),
				(SELECT COALESCE(MAX(id), 1) FROM ` + tbl + `))`); err != nil {
				t.Fatalf("sequence %s: %v", tbl, err)
			}
		}
	}
}

func balances(t *testing.T, h *db.Handles) map[string]int64 {
	t.Helper()
	rows, err := h.DB.Query(`
		SELECT la.code, COALESCE(SUM(le.amount), 0) FROM ledger_accounts la
		LEFT JOIN ledger_entries le ON le.account_id = la.id GROUP BY la.code`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			t.Fatal(err)
		}
		out[c] = v
	}
	return out
}

func scalar(t *testing.T, h *db.Handles, q string) int64 {
	t.Helper()
	var v int64
	if err := h.DB.QueryRow(q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestPerCurrencyMigration_MixedInstall(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	loadLegacy(t, h)
	if old := balances(t, h)["AFFILIATE_PAYABLE:1"]; old != 11000 {
		t.Fatalf("fixture: legacy mixed payable=%d want 11000", old)
	}
	entries := scalar(t, h, `SELECT COUNT(*) FROM ledger_entries`)

	if err := migrations.Apply(ctx, h.DB, dbtest.Engine()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	want := map[string]int64{
		"AFFILIATE_PAYABLE:1:USD": -9000, // $100.00 owed less the $10.00 clawed
		"AFFILIATE_PAYABLE:1:JPY": 20000, // the yen receivable, on its own
		"AFFILIATE_PAYABLE:2:USD": 0,
		"HOLD:USD":                -2000, // affiliate 2's pending $20.00
		"HOLD:JPY":                0,
		"EXPENSE:COMMISSION:USD":  16000,
		"EXPENSE:COMMISSION:JPY":  0,
		"PAYOUT_CLEARING:USD":     -5000,
		"PAYOUT_CLEARING:JPY":     -20000,
	}
	got := balances(t, h)
	for code, v := range want {
		if got[code] != v {
			t.Errorf("%s = %d want %d", code, got[code], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("accounts after upgrade: %v (legacy accounts must be gone)", got)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM ledger_entries`); n != entries {
		t.Fatalf("entries %d -> %d: the move must be lossless", entries, n)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM ledger_accounts WHERE currency IS NULL`); n != 0 {
		t.Fatalf("%d legacy accounts left", n)
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM (SELECT le.journal_id FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id
		GROUP BY le.journal_id HAVING COUNT(DISTINCT la.currency) > 1 OR SUM(le.amount) <> 0) j`); n != 0 {
		t.Fatalf("%d journals mixed or unbalanced after the move", n)
	}
	// The append-only guard is back.
	if _, err := h.DB.Exec(`UPDATE ledger_entries SET amount = amount WHERE id = 1`); err == nil {
		t.Fatal("ledger_entries updatable after the migration")
	}
	// Two currencies booked: the program currency stays USD.
	var prog string
	if err := h.DB.QueryRow(`SELECT currency FROM program_settings WHERE id = 1`).Scan(&prog); err != nil || prog != "USD" {
		t.Fatalf("program currency=%q err=%v want USD", prog, err)
	}

	// Re-run: a no-op.
	if err := migrations.Apply(ctx, h.DB, dbtest.Engine()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	again := balances(t, h)
	for code, v := range want {
		if again[code] != v {
			t.Errorf("re-run moved %s: %d want %d", code, again[code], v)
		}
	}
	if n := scalar(t, h, `SELECT COUNT(*) FROM audit_log WHERE action = 'rehome'`); n != 1 {
		t.Fatalf("rehome audit rows=%d want 1", n)
	}

	// The money the old balance hid now pays: the staged $100.00 payout is
	// released (a $10.00 refund landed after staging), the next batch pays
	// $90.00 and settles against the dollar payable alone. On the old
	// ledger that settle hit the overdraft guard (+11000 + 9000 > 0).
	p := ledger.NewPoster(h.Ledger)
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := p.Post(ctx, payouts.SettleKey(payoutID), "payout", "", time.Now().UTC(),
			payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })
	r := payouts.NewRunner(h.DB)
	if err := r.Approve(ctx, 2, nil); !errors.Is(err, payouts.ErrRestaged) {
		t.Fatalf("approve staged USD payout: %v want ErrRestaged", err)
	}
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v want 1 (USD only; the yen receivable waits)", n, err)
	}
	var pid, amount int64
	var cur string
	if err := h.DB.QueryRow(`SELECT id, amount, currency FROM payouts WHERE status = 'requested'`).Scan(&pid, &amount, &cur); err != nil {
		t.Fatal(err)
	}
	if amount != 9000 || cur != "USD" {
		t.Fatalf("payout %d %s want 9000 USD", amount, cur)
	}
	if err := r.Approve(ctx, pid, func(int64, int64) error { return nil }); err != nil {
		t.Fatalf("approve: %v", err)
	}
	after := balances(t, h)
	if after["AFFILIATE_PAYABLE:1:USD"] != 0 || after["AFFILIATE_PAYABLE:1:JPY"] != 20000 {
		t.Fatalf("after payout: USD=%d JPY=%d want 0 and 20000",
			after["AFFILIATE_PAYABLE:1:USD"], after["AFFILIATE_PAYABLE:1:JPY"])
	}
}

// A journal nothing attributes, in an install that booked two currencies,
// fails the upgrade naming the journal, and changes nothing.
func TestPerCurrencyMigration_UnattributableFailsLoudly(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	loadLegacy(t, h,
		`INSERT INTO journal_entries (id, idem_key, source, description, effective_at, created_at)
		 VALUES (99, 'manual:adjust-7', 'adjustment', 'hand-posted', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
		`INSERT INTO ledger_entries (id, journal_id, account_id, amount, created_at) VALUES (99, 99, 2, 700, '2026-10-01T00:00:00Z')`,
		`INSERT INTO ledger_entries (id, journal_id, account_id, amount, created_at) VALUES (100, 99, 1, -700, '2026-10-01T00:00:00Z')`)
	before := balances(t, h)

	err := migrations.Apply(ctx, h.DB, dbtest.Engine())
	if err == nil || !strings.Contains(err.Error(), "99 (manual:adjust-7)") {
		t.Fatalf("upgrade with an unattributable journal: %v (want a failure naming journal 99)", err)
	}
	after := balances(t, h)
	if len(after) != len(before) {
		t.Fatalf("accounts changed by a failed migration: %v -> %v", before, after)
	}
	for code, v := range before {
		if after[code] != v {
			t.Fatalf("%s moved by a failed migration: %d -> %d", code, v, after[code])
		}
	}
	if _, err := h.DB.Exec(`UPDATE ledger_entries SET amount = amount WHERE id = 1`); err == nil {
		t.Fatal("ledger_entries left updatable by a failed migration")
	}
}

// A single-currency install: a hand-posted journal can only be in its one
// currency, and a yen-only install's program currency becomes JPY (its
// fixed rules and tiers keep meaning yen).
func TestPerCurrencyMigration_SingleCurrencyInstall(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'yen@t.dev')`,
		`INSERT INTO ledger_accounts (id, code, kind) VALUES (1, 'HOLD', 'liability'), (2, 'EXPENSE:COMMISSION', 'expense')`,
		`INSERT INTO journal_entries (id, idem_key, source, description, effective_at, created_at)
		 VALUES (1, 'commission:stripe:ch_1', 'conversion', '', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
		        (2, 'manual:adjust-1', 'adjustment', '', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
		`INSERT INTO ledger_entries (id, journal_id, account_id, amount, created_at) VALUES
		 (1, 1, 2, 500, '2026-10-01T00:00:00Z'), (2, 1, 1, -500, '2026-10-01T00:00:00Z'),
		 (3, 2, 2, 100, '2026-10-01T00:00:00Z'), (4, 2, 1, -100, '2026-10-01T00:00:00Z')`,
		`INSERT INTO commissions (id, affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status)
		 VALUES (1, 1, 1, 1, 500, 'jpy', '{}', 'pending')`,
	} {
		if _, err := h.DB.Exec(s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	syncSequences(t, h)
	if err := migrations.Apply(ctx, h.DB, dbtest.Engine()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got := balances(t, h)
	if got["HOLD:JPY"] != -600 || got["EXPENSE:COMMISSION:JPY"] != 600 || len(got) != 2 {
		t.Fatalf("balances %v want HOLD:JPY -600, EXPENSE:COMMISSION:JPY 600", got)
	}
	var prog, cur string
	if err := h.DB.QueryRow(`SELECT currency FROM program_settings WHERE id = 1`).Scan(&prog); err != nil || prog != "JPY" {
		t.Fatalf("program currency=%q err=%v want JPY", prog, err)
	}
	if err := h.DB.QueryRow(`SELECT currency FROM commissions WHERE id = 1`).Scan(&cur); err != nil || cur != "JPY" {
		t.Fatalf("commission currency=%q err=%v want JPY (normalized)", cur, err)
	}
}
