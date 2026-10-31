package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

// restoreEnv: a migrated, money-carrying install plus a VACUUM INTO backup.
// Returns (livePath, backupPath). The live db has one commission (drill-1).
func restoreEnv(t *testing.T) (string, string) {
	t.Helper()
	live := testconvEnv(t) // migrated + affiliate 1 + 10% rule
	if err := runTestConversion(live, []string{"-affiliate", "1", "-external-id", "drill-1"}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := runBackup(live, backup); err != nil {
		t.Fatal(err)
	}
	return live, backup
}

// openCounts re-opens a db file and returns (commissions, ledgerSum, err).
// immutable=1: the rollback copy is a renamed WAL-mode live file whose -shm
// is gone (restore deletes the sidecars); plain mode=ro on a WAL-header file
// without -shm fails with SQLITE_IOERR(522). We only ever read test-owned
// files that nobody else is touching, so immutable is sound here.
func openCounts(path string) (int, int64, error) {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		return 0, 0, err
	}
	defer d.Close()
	var n int
	var sum int64
	if err := d.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n); err != nil {
		return 0, 0, err
	}
	if err := d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&sum); err != nil {
		return 0, 0, err
	}
	return n, sum, nil
}

// The full drill, as an invariant: backup a money-carrying install, destroy
// the live file, restore with --force, and prove the restored db (a) has the
// backup's commissions, (b) still balances, and (c) still rejects replays of
// events the backup already processed — idempotency survives the round-trip.
func TestRestore_Force_RoundTripsMoneyAndIdempotency(t *testing.T) {
	live, backup := restoreEnv(t)
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := runRestore(backup, live, true); err != nil {
		t.Fatal(err)
	}
	n, sum, err := openCounts(live)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || sum != 0 {
		t.Fatalf("restored commissions=%d (want 1), ledger sum=%d (want 0)", n, sum)
	}
	// Replay the pre-backup event id: must be rejected exactly as before.
	err = runTestConversion(live, []string{"-affiliate", "1", "-external-id", "drill-1"})
	if err == nil || !strings.Contains(err.Error(), "already processed") {
		t.Fatalf("err=%v want replay rejection after restore", err)
	}
}

// Restoring over an EXISTING live db: the swap path must preserve the old
// file as a timestamped rollback copy and promote the backup in its place.
func TestRestore_OverLive_PreservesRollbackCopy(t *testing.T) {
	live, backup := restoreEnv(t)
	// Post-backup write on live: the live db now has MORE than the backup.
	if err := runTestConversion(live, []string{"-affiliate", "1", "-external-id", "drill-2"}); err != nil {
		t.Fatal(err)
	}
	if err := runRestore(backup, live, false); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(live + ".pre-restore-*")
	if len(matches) != 1 {
		t.Fatalf("rollback copies=%d (want 1): %v", len(matches), matches)
	}
	n, _, err := openCounts(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rollback copy commissions=%d (want 2 — it must hold the pre-restore state)", n)
	}
	n, _, err = openCounts(live)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored live commissions=%d (want 1 — the backup's state)", n)
	}
}

// No live db and no --force: refuse, and do NOT leave a live file behind.
func TestRestore_NoLiveDB_RefusesWithoutForce(t *testing.T) {
	live, backup := restoreEnv(t)
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	err := runRestore(backup, live, false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err=%v want --force guidance", err)
	}
	if _, statErr := os.Stat(live); !os.IsNotExist(statErr) {
		t.Fatal("refused restore must not create a live db")
	}
}

// REGRESSION (found by the 2026-09-24 live drill): a stale -wal sibling left
// next to a lost live db is the crash artifact --force exists for. Before the
// fix, restore --force skipped sidecar removal and SQLite silently replayed
// the FOREIGN WAL onto the restored file: reads returned the OLD database's
// state, integrity_check "ok", zero errors. The restored db must instead be
// byte-faithful to the backup, and the stale sidecar must be gone.
func TestRestore_Force_RemovesStaleWAL(t *testing.T) {
	live, backup := restoreEnv(t)

	// Post-backup write whose frames live in live.db-wal: hold a WAL-mode
	// conn open, write through it, snapshot the wal bytes BEFORE any clean
	// close can checkpoint them away.
	h, err := db.Open(context.Background(), "sqlite:"+live, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO raw_events (source, external_event_id, event_type, payload, processed_at)
		VALUES ('drill', 'stale-wal-marker', 'purchase', '{}', '2026-09-24T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	walBytes, err := os.ReadFile(live + "-wal") // #nosec G304 -- test-owned t.TempDir path
	if err != nil {
		t.Fatal(err)
	}
	if len(walBytes) == 0 {
		t.Skip("driver checkpointed the WAL before it could be snapshotted — cannot fabricate the crash artifact")
	}
	h.DB.Close()

	// The disaster: live main file lost (crash), its WAL sibling survives.
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live+"-wal", walBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Restore from the backup with --force (the only path that applies).
	if err := runRestore(backup, live, true); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(live + "-wal"); !os.IsNotExist(statErr) {
		t.Fatal("stale -wal survived restore --force")
	}
	// And the restored db must NOT contain the stale-WAL marker row:
	// if the foreign WAL had replayed, this read would see it.
	n, _, err := openCounts(live)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored commissions=%d (want 1 — backup state)", n)
	}
	d, err := sql.Open("sqlite", "file:"+live+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var markers int
	if err := d.QueryRow(`SELECT COUNT(*) FROM raw_events WHERE external_event_id = 'stale-wal-marker'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatal("foreign WAL replayed onto the restored db — stale-wal marker row is present")
	}
}

// The forensic-pair path: when the live main file cannot be checkpointed
// (here: corrupted — the exact disaster an operator is restoring from),
// the rollback shell alone would be EMPTY. The live -wal/-shm must travel
// beside the rollback copy, never stay next to the live path, and never be
// silently destroyed.
func TestRestore_OverLive_Uncheckpointable_PreservesSidecarPair(t *testing.T) {
	live, backup := restoreEnv(t)
	// Corrupt the live main file but leave sidecars in place (crash shape).
	if err := os.WriteFile(live, []byte("corrupted main file — not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live+"-wal", []byte("residual wal frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live+"-shm", []byte("shm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRestore(backup, live, false); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(live + ".pre-restore-*")
	var prev string
	for _, m := range matches {
		if !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			prev = m
		}
	}
	if prev == "" {
		t.Fatalf("no rollback copy: %v", matches)
	}
	if _, err := os.Stat(prev + "-wal"); err != nil {
		t.Fatalf("forensic -wal not preserved beside rollback copy: %v", err)
	}
	if _, err := os.Stat(prev + "-shm"); err != nil {
		t.Fatalf("forensic -shm not preserved beside rollback copy: %v", err)
	}
	// The live path must carry the backup's state, un-poisoned.
	if _, err := os.Stat(live + "-wal"); !os.IsNotExist(err) {
		t.Fatal("stale -wal left next to the restored live db")
	}
	n, _, err := openCounts(live)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored commissions=%d (want 1 — backup state)", n)
	}
}

// A backup with stray -wal/-shm siblings looks like a LIVE db, not a VACUUM
// INTO output; restore must refuse it rather than restore half-state.
func TestRestore_RefusesBackupWithWALSiblings(t *testing.T) {
	live, backup := restoreEnv(t)
	if err := os.WriteFile(backup+"-wal", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runRestore(backup, live, false)
	if err == nil || !strings.Contains(err.Error(), "WAL") {
		t.Fatalf("err=%v want WAL-sibling refusal", err)
	}
}

// An unbalanced "backup" (ledger sum != 0) must be refused — restore never
// promotes unbalanced books. Fabricated directly: the invariant triggers make
// this state unreachable through the engine itself.
func TestRestore_RefusesUnbalancedBackup(t *testing.T) {
	live, _ := restoreEnv(t)
	fake := filepath.Join(t.TempDir(), "fake-backup.db")
	d, err := sql.Open("sqlite", "file:"+fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE ledger_entries (amount INTEGER NOT NULL);
		INSERT INTO ledger_entries VALUES (500), (500)`); err != nil { // sum=1000 ≠ 0
		t.Fatal(err)
	}
	d.Close()
	err = runRestore(fake, live, false)
	if err == nil || !strings.Contains(err.Error(), "not 0") {
		t.Fatalf("err=%v want unbalanced-ledger refusal", err)
	}
}

// Garbage as backup: integrity_check fails, restore refuses.
func TestRestore_RefusesNonDatabase(t *testing.T) {
	live, _ := restoreEnv(t)
	fake := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(fake, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runRestore(fake, live, false)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("err=%v want integrity refusal", err)
	}
}

// The staged copy must pass verification before promotion: a healthy SQLite
// file that is NOT a migrated engine db (too few tables) is refused.
func TestRestore_RefusesNonEngineDatabase(t *testing.T) {
	live, _ := restoreEnv(t)
	fake := filepath.Join(t.TempDir(), "empty.db")
	d, err := sql.Open("sqlite", "file:"+fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	err = runRestore(fake, live, false)
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("err=%v want staged-verification refusal", err)
	}
	// The live db must be untouched (no rollback copy, no swap).
	matches, _ := filepath.Glob(live + ".pre-restore-*")
	if len(matches) != 0 {
		t.Fatalf("refused restore must not touch the live db; rollback copies=%v", matches)
	}
}

// A backup file is untrusted input: one whose guard triggers were dropped or
// replaced by a no-op under the same name must not come back to life with
// the guards gutted. Restore re-applies the migration chain on the staged
// copy, so the promoted db refuses ledger mutation again.
func TestRestore_ReappliesGuardTriggers(t *testing.T) {
	live, backup := restoreEnv(t)
	d, err := sql.Open("sqlite", "file:"+backup+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DROP TRIGGER trg_ledger_entries_no_delete;
		CREATE TRIGGER trg_ledger_entries_no_delete BEFORE DELETE ON ledger_entries BEGIN SELECT 1; END;
		DROP TRIGGER trg_journal_no_update`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	if err := runRestore(backup, live, false); err != nil {
		t.Fatal(err)
	}
	l, err := sql.Open("sqlite", "file:"+live)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Exec(`DELETE FROM ledger_entries`); err == nil || !strings.Contains(err.Error(), "ledger_immutable") {
		t.Fatalf("restored db accepted a ledger DELETE (guard still neutered): %v", err)
	}
	if _, err := l.Exec(`UPDATE journal_entries SET source = 'tampered'`); err == nil {
		t.Fatal("restored db accepted a journal UPDATE (guard missing)")
	}
}
