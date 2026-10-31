-- Wave 2 additions: payout pipeline tables (both dialects, SQLite shown).
-- Applied by migrations.Apply as the third script.

CREATE TABLE IF NOT EXISTS payout_runs (
    id          INTEGER PRIMARY KEY,
    status      TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running','completed','failed')),
    started_at  TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at TEXT,
    note        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS payout_run_lines (
    run_id      INTEGER NOT NULL REFERENCES payout_runs(id),
    payout_id   INTEGER NOT NULL REFERENCES payouts(id),
    PRIMARY KEY (run_id, payout_id)
);
