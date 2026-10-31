-- Fraud verdicts table (both dialects, SQLite shown).

CREATE TABLE IF NOT EXISTS fraud_verdicts (
    id          INTEGER PRIMARY KEY,
    rule        TEXT NOT NULL,
    subject     TEXT NOT NULL,
    reason      TEXT NOT NULL,
    action      INTEGER NOT NULL,
    dry_run     INTEGER NOT NULL DEFAULT 0,
    resolved    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS ix_fraud_verdicts_open ON fraud_verdicts(id)
    WHERE resolved = 0;

-- Fraud hold on commissions: blocks payout inclusion while under review.
-- Does NOT touch the state machine (status unchanged); payout batching
-- skips held commissions until resolved.
CREATE TABLE IF NOT EXISTS commission_holds (
    commission_id INTEGER NOT NULL PRIMARY KEY REFERENCES commissions(id),
    reason        TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at   TEXT,
    resolution    TEXT CHECK (resolution IN ('released','upheld')),
    note          TEXT NOT NULL DEFAULT ''
);
