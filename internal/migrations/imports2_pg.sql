-- Importer batch state (PostgreSQL dialect; mirror of imports2_sqlite.sql).

CREATE TABLE IF NOT EXISTS import_batch_state (
    batch_id    BIGINT PRIMARY KEY REFERENCES import_batches(id),
    fingerprint TEXT NOT NULL DEFAULT '',
    undone_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_import_batch_state_fp ON import_batch_state(fingerprint);
