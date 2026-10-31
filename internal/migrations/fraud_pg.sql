-- Fraud verdicts table (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS fraud_verdicts (
    id          BIGSERIAL PRIMARY KEY,
    rule        TEXT NOT NULL,
    subject     TEXT NOT NULL,
    reason      TEXT NOT NULL,
    action      INTEGER NOT NULL,
    dry_run     BOOLEAN NOT NULL DEFAULT false,
    resolved    BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_fraud_verdicts_open ON fraud_verdicts(id)
    WHERE resolved = false;

CREATE TABLE IF NOT EXISTS commission_holds (
    commission_id BIGINT NOT NULL PRIMARY KEY REFERENCES commissions(id),
    reason        TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    resolution    TEXT CHECK (resolution IN ('released','upheld')),
    note          TEXT NOT NULL DEFAULT ''
);
