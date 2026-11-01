-- Per billing tool webhook outcomes. PG mirror of webhookhealth_sqlite.sql.
CREATE TABLE IF NOT EXISTS webhook_health (
    provider    TEXT PRIMARY KEY,
    last_ok_at  TEXT,
    last_err_at TEXT,
    last_err    TEXT,
    ok_count    BIGINT NOT NULL DEFAULT 0,
    err_count   BIGINT NOT NULL DEFAULT 0
);

-- Signature failures, kept apart from processing failures (see the SQLite
-- script).
ALTER TABLE webhook_health ADD COLUMN IF NOT EXISTS last_sigfail_at TEXT;
ALTER TABLE webhook_health ADD COLUMN IF NOT EXISTS sigfail_count BIGINT NOT NULL DEFAULT 0;

UPDATE webhook_health
SET last_sigfail_at = last_err_at, last_err_at = NULL, last_err = NULL
WHERE last_err LIKE 'billing: invalid webhook signature%';
