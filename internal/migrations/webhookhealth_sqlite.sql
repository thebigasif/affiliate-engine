-- Per billing tool: when its webhook last delivered an event the engine
-- accepted, and when it last rejected a signed one (and why). The dashboard
-- uses it to warn when sales stop being credited.
CREATE TABLE IF NOT EXISTS webhook_health (
    provider    TEXT PRIMARY KEY,
    last_ok_at  TEXT,
    last_err_at TEXT,
    last_err    TEXT,
    ok_count    INTEGER NOT NULL DEFAULT 0,
    err_count   INTEGER NOT NULL DEFAULT 0
);
