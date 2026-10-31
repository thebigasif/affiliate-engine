-- Affiliate portal auth: sessions + application state (both waves' base).
-- SQLite dialect (pg mirror in portal_pg.sql).

CREATE TABLE IF NOT EXISTS affiliate_sessions (
    token_hash   TEXT PRIMARY KEY,          -- hex(SHA-256(token))
    affiliate_id INTEGER NOT NULL REFERENCES affiliates(id),
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at   TEXT NOT NULL,
    revoked_at   TEXT
);
CREATE INDEX IF NOT EXISTS ix_aff_sessions_exp ON affiliate_sessions(expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS affiliate_applications (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','rejected')),
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    reviewed_at TEXT
);

CREATE TABLE IF NOT EXISTS portal_login_grants (
    token_hash   TEXT PRIMARY KEY,
    affiliate_id INTEGER NOT NULL REFERENCES affiliates(id),
    expires_at   TEXT NOT NULL,
    used_at      TEXT
);
