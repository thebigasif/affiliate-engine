-- Importer bookkeeping (PostgreSQL dialect; mirror of imports_sqlite.sql).

CREATE TABLE IF NOT EXISTS import_batches (
    id          BIGSERIAL PRIMARY KEY,
    source      TEXT NOT NULL,
    dry_run     BOOLEAN NOT NULL DEFAULT false,
    affiliates  INTEGER NOT NULL,
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS import_undos (
    batch_id  BIGINT PRIMARY KEY REFERENCES import_batches(id),
    undo_json JSONB NOT NULL
);
