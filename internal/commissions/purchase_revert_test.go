package commissions_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// P0-3 pin: if the commission-row INSERT fails, the credit journal must
// not survive it (HOLD not inflated). It used to be repaired by an inverse
// journal; the journal and the row now share one transaction, so the
// failed attempt leaves no journal at all. Simulated with a
// BEFORE INSERT trigger that aborts, then removed for the retry check.
//
// Ledger reads JOIN ledger_accounts on code — the naive
// `WHERE account='HOLD:USD'` hits a nonexistent column and Scan swallows
// the error into 0, making balance assertions vacuous.
func holdBalance(t *testing.T, d *sql.DB, code string) int64 {
	t.Helper()
	var v int64
	err := d.QueryRow(`SELECT COALESCE(SUM(le.amount),0)
		FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.code = ?`, code).Scan(&v)
	if err != nil {
		t.Fatalf("holdBalance(%s): %v", code, err)
	}
	return v
}

func TestProcessPurchase_InsertFailureReverts(t *testing.T) {
	e := newEnv(t)
	h, eng := e.h, e.e
	ctx := context.Background()

	// Abort every commissions insert.
	if dbtest.IsPostgres() {
		if _, err := h.DB.Exec(`CREATE OR REPLACE FUNCTION kill_ins() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'inject'; END $$ LANGUAGE plpgsql`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`CREATE TRIGGER kill_ins BEFORE INSERT ON commissions
			FOR EACH ROW EXECUTE FUNCTION kill_ins()`); err != nil {
			t.Fatal(err)
		}
	} else if _, err := h.DB.Exec(`CREATE TRIGGER kill_ins BEFORE INSERT ON commissions
		BEGIN SELECT RAISE(ABORT, 'inject'); END`); err != nil {
		t.Fatal(err)
	}
	ev := purchase("p03rev", 10000)
	_, err := eng.ProcessPurchase(ctx, ev)
	if err == nil {
		t.Fatal("expected insert failure")
	}
	if dbtest.IsPostgres() {
		h.DB.Exec(`DROP TRIGGER IF EXISTS kill_ins ON commissions`)
	} else {
		h.DB.Exec(`DROP TRIGGER kill_ins`)
	}

	// HOLD must be untouched: the credit rolled back with the row.
	if hold := holdBalance(t, h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("P0-3: HOLD=%d after failed insert (credit journal survived)", hold)
	}
	var s int64
	if err := h.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != 0 {
		t.Fatalf("ledger not balanced: %d", s)
	}

	// The failed event was NOT marked processed, so the provider's retry
	// with the SAME event id completes it. (It used to be refused as
	// ErrAlreadyProcessed: the raw_events row was inserted first, so any
	// later failure dropped the commission forever.)
	cid, err := eng.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatalf("same-id retry: %v", err)
	}
	if cid == 0 {
		t.Fatal("no commission on same-id retry")
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE amount=1000`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("commissions after retry=%d want 1", n)
	}
	// The failed attempt spent nothing, so the retry credited under the
	// original key: exactly one credit journal, no revert.
	var credits int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries
		WHERE idem_key LIKE 'commission:stripe:p03rev%'`).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if credits != 1 {
		t.Fatalf("credit journals=%d want 1 (the failed attempt rolled back)", credits)
	}
	// Net HOLD: the failed attempt contributed 0; the retry holds -1000
	// until maturation drains it.
	if hold := holdBalance(t, h.DB, "HOLD:USD"); hold != -1000 {
		t.Fatalf("HOLD after retry=%d want -1000 (the one real commission, posted as -amount)", hold)
	}
	// Once finished, a replay is refused.
	if _, err := eng.ProcessPurchase(ctx, ev); err != commissions.ErrAlreadyProcessed {
		t.Fatalf("replay of finished event: %v (want ErrAlreadyProcessed)", err)
	}
}
