package migrations_test

// Guard-trigger pins (dual-engine via dbtest): money rows refuse the
// mutations no production path makes, the one legitimate delete (the
// payout runner's prune) still works, and re-applying the chain heals a
// tampered trigger.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

func mustGuardExec(t *testing.T, h *db.Handles, q string, args ...any) {
	t.Helper()
	if _, err := h.DB.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func expectGuard(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("err=%v, want guard %q", err, msg)
	}
}

// bookedCommission posts a real journal (with ledger lines) and inserts an
// approved commission on it.
func bookedCommission(t *testing.T, h *db.Handles, idem string) int64 {
	t.Helper()
	jid, err := ledger.NewPoster(h.Ledger).Post(context.Background(), idem, "conversion", "",
		time.Now().UTC(), []ledger.Line{
			{Account: "EXPENSE:COMMISSION:USD", Amount: 1000},
			{Account: "HOLD:USD", Amount: -1000},
		})
	if err != nil {
		t.Fatal(err)
	}
	var cid int64
	if err := h.DB.QueryRow(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (1, ?, 1000, '{}', 'approved') RETURNING id`, jid).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	return cid
}

// guardWorld: affiliate 1, one ledger-backed approved commission staged
// into one requested payout.
func guardWorld(t *testing.T) (*db.Handles, int64, int64) {
	t.Helper()
	h := dbtest.Open(t)
	mustGuardExec(t, h, `INSERT INTO affiliates (id, email) VALUES (1, 'guard@t.dev')`)
	cid := bookedCommission(t, h, "guard:1")
	var pid int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount) VALUES (1, 1000) RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	mustGuardExec(t, h, `INSERT INTO payout_lines (payout_id, commission_id) VALUES (?, ?)`, pid, cid)
	return h, cid, pid
}

func TestGuards_JournalNoUpdate(t *testing.T) {
	h, _, _ := guardWorld(t)
	_, err := h.DB.Exec(`UPDATE journal_entries SET idem_key = idem_key || '-renamed'`)
	expectGuard(t, err, "ledger_immutable")
}

func TestGuards_CommissionNoDelete(t *testing.T) {
	h, _, _ := guardWorld(t)
	cid := bookedCommission(t, h, "guard:2") // not staged: no FK in the way
	_, err := h.DB.Exec(`DELETE FROM commissions WHERE id = ?`, cid)
	expectGuard(t, err, "commission_immutable")

	// A row on a journal with no ledger lines books no money: deletable.
	var jid int64
	if err := h.DB.QueryRow(`INSERT INTO journal_entries (idem_key, source, effective_at)
		VALUES ('guard:raw', 'test', ?) RETURNING id`, time.Now().UTC()).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	var raw int64
	if err := h.DB.QueryRow(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (1, ?, 5, '{}', 'pending') RETURNING id`, jid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	mustGuardExec(t, h, `DELETE FROM commissions WHERE id = ?`, raw)
}

func TestGuards_PayoutNoDelete(t *testing.T) {
	h, _, _ := guardWorld(t)
	var pid int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount) VALUES (1, 50) RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	_, err := h.DB.Exec(`DELETE FROM payouts WHERE id = ?`, pid)
	expectGuard(t, err, "payout_immutable")
}

// The runner's prune is the one legitimate payout_lines delete: payout
// 'processing' AND commission cancelled/clawed back. Anything else refuses.
func TestGuards_PayoutLinesOnlyRunnerPrune(t *testing.T) {
	h, cid, pid := guardWorld(t)
	_, err := h.DB.Exec(`DELETE FROM payout_lines WHERE payout_id = ?`, pid)
	expectGuard(t, err, "payout_line_immutable")

	mustGuardExec(t, h, `UPDATE commissions SET status = 'cancelled' WHERE id = ?`, cid)
	_, err = h.DB.Exec(`DELETE FROM payout_lines WHERE payout_id = ?`, pid)
	expectGuard(t, err, "payout_line_immutable") // payout still only 'requested'

	mustGuardExec(t, h, `UPDATE payouts SET status = 'processing' WHERE id = ?`, pid)
	mustGuardExec(t, h, `DELETE FROM payout_lines WHERE payout_id = ?`, pid)
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines WHERE payout_id = ?`, pid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lines=%d err=%v, want pruned", n, err)
	}
}

func TestGuards_PayoutStatusMachine(t *testing.T) {
	h, _, pid := guardWorld(t)
	_, err := h.DB.Exec(`UPDATE payouts SET status = 'paid' WHERE id = ?`, pid)
	expectGuard(t, err, "illegal_payout_transition") // requested -> paid skips processing

	// The runner's real path: CAS in, revert for retry, CAS in, settle.
	for _, st := range []string{"processing", "requested", "processing", "paid"} {
		mustGuardExec(t, h, `UPDATE payouts SET status = ? WHERE id = ?`, st, pid)
	}
	_, err = h.DB.Exec(`UPDATE payouts SET status = 'requested' WHERE id = ?`, pid)
	expectGuard(t, err, "illegal_payout_transition") // paid is terminal

	// requested -> approved stays legal (admin two-step flow).
	var p2 int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount) VALUES (1, 70) RETURNING id`).Scan(&p2); err != nil {
		t.Fatal(err)
	}
	mustGuardExec(t, h, `UPDATE payouts SET status = 'approved' WHERE id = ?`, p2)
}

func TestGuards_TruncateBlocked(t *testing.T) {
	if !dbtest.IsPostgres() {
		t.Skip("TRUNCATE guards are PG-only; SQLite has no TRUNCATE (DELETE triggers cover it)")
	}
	h, _, _ := guardWorld(t)
	var before int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM ledger_entries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"ledger_entries", "journal_entries", "audit_log", "commissions",
		"payouts", "payout_lines", "raw_events",
	} {
		if _, err := h.DB.Exec(`TRUNCATE ` + table + ` CASCADE`); err == nil {
			t.Fatalf("TRUNCATE %s accepted", table)
		}
	}
	var after int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM ledger_entries`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before || before == 0 {
		t.Fatalf("ledger rows before=%d after=%d", before, after)
	}
}

// A neutered guard (dropped, or re-created as a no-op under the same name)
// is restored by the next Apply — the guards script is authoritative.
func TestGuards_ReapplyHealsTamperedTrigger(t *testing.T) {
	h, _, _ := guardWorld(t)
	if dbtest.IsPostgres() {
		mustGuardExec(t, h, `DROP TRIGGER trg_ledger_entries_no_delete ON ledger_entries`)
	} else {
		mustGuardExec(t, h, `DROP TRIGGER trg_ledger_entries_no_delete`)
		mustGuardExec(t, h, `CREATE TRIGGER trg_ledger_entries_no_delete BEFORE DELETE ON ledger_entries BEGIN SELECT 1; END`)
	}
	if err := migrations.Apply(context.Background(), h.DB, dbtest.Engine()); err != nil {
		t.Fatal(err)
	}
	_, err := h.DB.Exec(`DELETE FROM ledger_entries`)
	expectGuard(t, err, "ledger_immutable")
}

// Every name restore verifies must actually be (re)created by the script.
func TestGuards_ListMatchesScript(t *testing.T) {
	b, err := migrations.Files().ReadFile("guards_sqlite.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, name := range migrations.SQLiteGuardTriggers {
		if !strings.Contains(s, "DROP TRIGGER IF EXISTS "+name+";") {
			t.Errorf("guards_sqlite.sql does not drop+recreate %s", name)
		}
	}
}
