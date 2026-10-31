-- Postback delivery state (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS postback_state (
    commission_id    BIGINT NOT NULL PRIMARY KEY REFERENCES commissions(id),
    attempts         INTEGER NOT NULL DEFAULT 0,
    delivered_at     TIMESTAMPTZ,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    note             TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_postback_retry ON postback_state(next_attempt_at)
    WHERE delivered_at IS NULL AND attempts < 5;
