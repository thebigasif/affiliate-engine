-- Postback delivery state (SQLite dialect).

CREATE TABLE IF NOT EXISTS postback_state (
    commission_id    INTEGER NOT NULL PRIMARY KEY REFERENCES commissions(id),
    attempts         INTEGER NOT NULL DEFAULT 0,
    delivered_at     TEXT,
    next_attempt_at  TEXT NOT NULL DEFAULT (datetime('now')),
    note             TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_postback_retry ON postback_state(next_attempt_at)
    WHERE delivered_at IS NULL AND attempts < 5;
