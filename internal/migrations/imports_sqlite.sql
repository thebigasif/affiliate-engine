-- Importer bookkeeping (SQLite dialect): batches + rollback payloads.
-- Money rows are never imported (documented contract) — only identity,
-- codes (slug preservation), and coupons.

CREATE TABLE IF NOT EXISTS import_batches (
    id          INTEGER PRIMARY KEY,
    source      TEXT NOT NULL,               -- 'rewardful' | 'firstpromoter' | ...
    dry_run     INTEGER NOT NULL DEFAULT 0,
    affiliates  INTEGER NOT NULL,
    imported_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS import_undos (
    batch_id  INTEGER PRIMARY KEY REFERENCES import_batches(id),
    undo_json TEXT NOT NULL
);
