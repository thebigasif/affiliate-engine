// restore.go — the `restore` subcommand: disaster recovery for the SQLite
// engine. Backup without restore is half a feature (audit NFR-6: "restore
// drills impossible"); this closes it.
//
// Protocol (crash-safe):
//  1. verify the backup file is a healthy SQLite DB (PRAGMA integrity_check)
//  2. verify the WAL siblings are NOT required (VACUUM INTO outputs are
//     self-contained; refuse -wal/-shm remnants to avoid half-state)
//  3. copy backup -> <target>.restore-tmp (same filesystem, atomic rename)
//  4. doctor the restored copy BEFORE it replaces the live file, and
//     re-apply the migration chain so its guard triggers are the
//     authoritative ones (a restored file never keeps neutered guards)
//  5. rename live -> <target>.pre-restore-<ts> (instant rollback path)
//  6. rename tmp -> live
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

func runRestore(backupPath, dbPath string, force bool) error {
	if backupPath == "" || dbPath == "" {
		return fmt.Errorf("restore requires -from <backup.db> and -db sqlite:<path>")
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("restore: backup file: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(backupPath + suffix); err == nil {
			return fmt.Errorf("restore: %s exists next to the backup — a VACUUM INTO backup is self-contained; stray WAL siblings suggest this is a live DB, not a backup", backupPath+suffix)
		}
	}

	// 1) The backup must be a healthy SQLite database.
	chk, err := sql.Open("sqlite", "file:"+backupPath+"?mode=ro")
	if err != nil {
		return err
	}
	var integrity string
	if err := chk.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		_ = chk.Close()
		return fmt.Errorf("restore: backup integrity: %q (err=%v) — refusing", integrity, err)
	}
	// Ledger proof on the backup itself: the restored state must balance.
	var sum int64
	err = chk.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&sum)
	_ = chk.Close()
	if err == nil && sum != 0 {
		return fmt.Errorf("restore: backup ledger sums to %d, not 0 — refusing to restore unbalanced books", sum)
	}

	// 2) Stage the copy on the same filesystem (atomic rename target).
	tmp := dbPath + ".restore-tmp"
	if err := copyFile(backupPath, tmp); err != nil {
		return fmt.Errorf("restore: stage: %w", err)
	}

	// 3) Doctor the staged copy before it goes live.
	if err := verifyStaged(tmp); err != nil {
		removeStaged(tmp)
		return fmt.Errorf("restore: staged copy failed verification: %w", err)
	}

	// 4) Swap: live -> timestamped rollback copy; tmp -> live.
	liveExists := false
	if _, err := os.Stat(dbPath); err == nil {
		liveExists = true
	}
	if liveExists {
		// Checkpoint FIRST: in WAL mode the live main file can be a near-
		// empty shell while the entire database sits in -wal (measured on
		// a quiet install: 4096-byte main vs 1.7MB wal — auto-checkpoint
		// only fires at ~1000 dirty pages). Renaming the shell away and
		// dropping the sidecars would leave the rollback copy EMPTY.
		// Best effort: fold the WAL into the main file so the preserved
		// copy is self-contained. Failure is not fatal — a locked or
		// corrupt live db is precisely why operators restore — but then
		// the sidecars must travel with the rollback copy (below), not
		// be deleted, or the pre-restore state is destroyed.
		walFolded := checkpointLive(dbPath)
		prev := fmt.Sprintf("%s.pre-restore-%s", dbPath, time.Now().UTC().Format("20060102-150405"))
		if err := os.Rename(dbPath, prev); err != nil {
			removeStaged(tmp)
			return fmt.Errorf("restore: preserve live: %w", err)
		}
		fmt.Printf("restore: previous db preserved at %s\n", prev)
		if walFolded {
			// The main file is self-contained; any sidecar remnants are
			// empty post-checkpoint husks. Remove them — a rollback copy
			// should be one file.
			for _, suffix := range []string{"-wal", "-shm"} {
				_ = os.Remove(dbPath + suffix)
			}
		} else {
			// Checkpoint failed (locked/corrupt live db): the renamed
			// shell alone would be an EMPTY rollback copy. Keep the
			// sidecars beside it as a forensic pair — they are no longer
			// siblings of the live path, so they cannot poison the
			// restored db, and the old content stays recoverable.
			kept := false
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(dbPath + suffix); err == nil {
					if err := os.Rename(dbPath+suffix, prev+suffix); err == nil {
						kept = true
					} else {
						_ = os.Remove(dbPath + suffix) // never leave it next to the live path
					}
				}
			}
			if kept {
				fmt.Printf("restore: NOTE — live WAL could not be checkpointed; sidecars preserved beside the rollback copy for recovery\n")
			}
		}
	} else if !force {
		removeStaged(tmp)
		return fmt.Errorf("restore: no live db at %s (use --force to create from backup)", dbPath)
	}
	// Any WAL/SHM siblings still at the live path are invalid for the
	// restored file and MUST be removed on every path — including
	// --force (live db lost), which is precisely the crash scenario most
	// likely to leave a stale WAL behind. Probed for real: a foreign WAL
	// left next to the restored file is replayed onto it silently
	// (SQLite recovers frames when the page size matches and the
	// salt/checksum happen to align), so the engine reads the OLD
	// database's post-backup state while the operator believes they are
	// on the backup — integrity_check "ok", zero errors. A failed removal
	// (EBUSY etc.) must abort for the same reason.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restore: remove stale %s: %w", sidecar, err)
		}
	}

	if err := os.Rename(tmp, dbPath); err != nil {
		return fmt.Errorf("restore: promote: %w", err)
	}
	// Make the promote durable: a rename is metadata-only, and the
	// restored db's directory entry can otherwise be lost to a power
	// cut while the (now unlinked) old inode lingers — leaving no live
	// db at all. Fsyncing the containing directory flushes the rename.
	if err := fsyncDir(dbPath); err != nil {
		fmt.Printf("restore: WARNING — could not fsync directory (%v); the promote is not yet durable\n", err)
	}
	fmt.Printf("restore: %s -> %s complete (doctor the live db to confirm)\n", backupPath, dbPath)
	return nil
}

// removeStaged deletes the staged copy and any journal sidecars it grew.
func removeStaged(tmp string) {
	for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm", tmp + "-journal"} {
		_ = os.Remove(p)
	}
}

// verifyStaged runs the cheap critical checks on the staged copy: opens it,
// confirms the schema chain is present and the ledger balances, then
// re-applies the migration chain (guard triggers last) and confirms every
// guard trigger exists. A backup file is untrusted input: one whose guard
// was dropped or replaced by a no-op under the same name must not come
// back to life with the guard still gutted.
//
// The re-apply writes to the staged file, so it runs on ONE connection and
// finishes by switching the copy to rollback-journal mode: that checkpoints
// and removes any <tmp>-wal, so the file promoted by rename is
// self-contained (the rename moves only the main file).
func verifyStaged(path string) error {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	var tables int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		return err
	}
	if tables < 20 {
		return fmt.Errorf("only %d tables — not a migrated engine db", tables)
	}
	var sum int64
	if err := d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&sum); err != nil {
		return err
	}
	if sum != 0 {
		return fmt.Errorf("ledger sums to %d", sum)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migrations.Apply(ctx, d, migrations.SQLite); err != nil {
		return fmt.Errorf("re-apply migrations (guard triggers): %w", err)
	}
	for _, name := range migrations.SQLiteGuardTriggers {
		var body string
		if err := d.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&body); err != nil {
			return fmt.Errorf("guard trigger %s missing after re-apply: %w", name, err)
		}
	}
	var mode string
	if err := d.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return fmt.Errorf("fold staged journal: %w", err)
	}
	return nil
}

// checkpointLive best-effort folds a live db's WAL into its main file
// (TRUNCATE checkpoint). Returns true when the WAL is gone or empty
// afterwards — i.e. the main file is self-contained and safe to rename
// away as a rollback copy. On any error (locked, corrupt, not a db)
// it returns false and leaves the sidecars untouched for the caller's
// forensic-pair path.
func checkpointLive(dbPath string) bool {
	d, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(2000)")
	if err != nil {
		return false
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return false
	}
	st, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return true // no WAL at all — self-contained
	}
	return st.Size() == 0 // TRUNCATE zeroes the file on success
}

// fsyncDir flushes a directory's entries so renames performed in it survive
// a power cut. Errors are reported, not fatal: on filesystems where directory
// fsync is unsupported the caller degrades to a loud warning.
func fsyncDir(path string) error {
	dir := filepath.Dir(path)
	f, err := os.Open(dir) // #nosec G304 -- parent of the engine's own db path
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- operator-supplied CLI path (`restore -from <file>`); the whole command exists to copy that exact file
	if err != nil {
		return err
	}
	defer in.Close()
	// 0o600: the destination is (a copy of) the money database —
	// operator-only, no group/other read. SQLite itself creates dbs with
	// 0644 by default, but a restored ledger should not be world-readable.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- dst is the engine's own db path beside the live file
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	// The staged copy is about to become the live money database via a
	// metadata-only rename: unless its pages are flushed first, a power
	// cut after the promote can leave the live db truncated/empty —
	// during disaster recovery, of all times. fsync the payload before
	// the caller renames it into place.
	if err := out.Sync(); err != nil {
		return fmt.Errorf("fsync staged copy: %w", err)
	}
	return nil
}
