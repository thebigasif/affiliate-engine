-- Per billing tool webhook outcomes. PG mirror of webhookhealth_sqlite.sql.
CREATE TABLE IF NOT EXISTS webhook_health (
    provider    TEXT PRIMARY KEY,
    last_ok_at  TEXT,
    last_err_at TEXT,
    last_err    TEXT,
    ok_count    BIGINT NOT NULL DEFAULT 0,
    err_count   BIGINT NOT NULL DEFAULT 0
);
