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

-- Signature failures are kept apart from real processing failures: a
-- request that fails the signature check proves nothing about the billing
-- tool, so it must never turn billing health red. The dashboard shows them
-- at most as a quiet note.
ALTER TABLE webhook_health ADD COLUMN last_sigfail_at TEXT;
ALTER TABLE webhook_health ADD COLUMN sigfail_count INTEGER NOT NULL DEFAULT 0;

-- Older releases stored signature failures as errors. Move them over once
-- (the WHERE matches nothing after the first run, so this is idempotent).
UPDATE webhook_health
SET last_sigfail_at = last_err_at, last_err_at = NULL, last_err = NULL
WHERE last_err LIKE 'billing: invalid webhook signature%';
