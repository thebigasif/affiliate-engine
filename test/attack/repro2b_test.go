package attack_test

// Deterministic, PG-only regression for the nightly 2026-09-25 failure in
// TestAttack2b_ConcurrentSettlePairs (run 36112774239, flake-hunt (3)):
// "callers reporting payout=2, pairs_paid in DB=10".
//
// Mechanism: SettlePairs SELECTs pairs_paid, computes new pairs, then CASes
// `UPDATE ... WHERE pairs_paid = <read>`. On PG READ COMMITTED, when a
// concurrent winner commits first, the loser's UPDATE re-evaluates the
// predicate, matches ZERO rows, and still returns err=nil. Pre-fix, the
// unchecked RowsAffected made the loser return (pairs=10, payout=5000) —
// a payout that never happened; any caller posting from the return value
// pays twice. On SQLite (_txlock=immediate) the loser fails at BEGIN with
// BUSY instead, so this only manifests on PG.
//
// Orchestration (no timing race): txWinner UPDATEs the row and HOLDS the
// lock uncommitted; SettlePairs' SELECT sees the old value (READ COMMITTED
// never sees uncommitted writes), its CAS blocks on the row lock; txWinner
// commits; the CAS re-evaluates and matches 0 rows. Pre-fix SettlePairs
// returned (10, 5000, nil); post-fix it must return (0, 0, nil).

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

// waitFor polls fn until true or the timeout fires; failure is fatal.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

const timeSecond = time.Second

func TestRepro2b_DeterministicFalsePayoutInterleaving(t *testing.T) {
	e := newEnv(t)
	if !dbtest.IsPostgres() {
		t.Skip("deterministic interleaving is PG-specific; SQLite loses at BEGIN with BUSY")
	}
	ctx := context.Background()
	db := e.h.DB
	day := int64(20260101)

	// leg_l=10, leg_r=10, pairs_paid=0 — same state the attack test builds.
	mustExec(t, db, `INSERT INTO pair_volume (affiliate_id, day, leg_l, leg_r, pairs_paid, payout_cents)
		VALUES (1, ?, 10, 10, 0, 0)`, day)

	// The concurrent winner: takes the row lock, keeps it uncommitted.
	txWinner, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txWinner.Rollback()
	if _, err := txWinner.ExecContext(ctx,
		`UPDATE pair_volume SET pairs_paid = 10, payout_cents = 5000
		 WHERE affiliate_id = 1 AND day = ?`, day); err != nil {
		t.Fatal(err)
	}

	type result struct {
		pairs  int64
		payout int64
		err    error
	}
	done := make(chan result, 1)
	go func() {
		pairs, payout, err := mlm.New(db).SettlePairs(ctx, 1, day, mlm.PairConfig{PairPayoutCents: 500})
		done <- result{pairs, payout, err}
	}()

	// Give SettlePairs time to run its SELECT and block on the row lock.
	// Row-lock waiters appear in pg_locks as non-granted 'transactionid'
	// entries (the winner's XID), not as relation locks.
	waitFor(t, 5*timeSecond, func() bool {
		var n int64
		return db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pg_locks
			WHERE locktype = 'transactionid' AND NOT granted`).Scan(&n) == nil && n > 0
	})

	// Winner commits: SettlePairs' blocked CAS re-evaluates, matches 0 rows.
	if err := txWinner.Commit(); err != nil {
		t.Fatal(err)
	}

	r := <-done
	paid := qInt64(t, db, `SELECT pairs_paid FROM pair_volume WHERE affiliate_id=1 AND day=?`, day)
	t.Logf("SettlePairs returned (pairs=%d, payout=%d, err=%v); DB pairs_paid=%d",
		r.pairs, r.payout, r.err, paid)

	if r.err != nil {
		t.Fatalf("SettlePairs errored: %v (should be a clean CAS loss)", r.err)
	}
	if paid != 10 {
		t.Errorf("P1 pairs_paid=%d want 10 (double-consumed volume)", paid)
	}
	if r.pairs != 0 || r.payout != 0 {
		t.Errorf("P1 FALSE PAYOUT REPORT: loser returned (pairs=%d, payout=%d), want (0, 0) — a caller posting from the return value pays twice", r.pairs, r.payout)
	}
}
