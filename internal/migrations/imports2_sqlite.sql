-- Importer batch state (SQLite dialect; PG mirror in imports2_pg.sql).
-- A side table instead of new import_batches columns: the chain's
-- idempotent-ALTER convention skips the rest of a script once one ALTER
-- reports "duplicate column", so new state lives in its own table.
--   fingerprint: sha256 of the normalized import input (re-run detection)
--   undone_at:   set when `import -undo` reversed the batch

CREATE TABLE IF NOT EXISTS import_batch_state (
    batch_id    INTEGER PRIMARY KEY REFERENCES import_batches(id),
    fingerprint TEXT NOT NULL DEFAULT '',
    undone_at   TEXT
);
CREATE INDEX IF NOT EXISTS ix_import_batch_state_fp ON import_batch_state(fingerprint);
