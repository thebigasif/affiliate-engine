-- API keys (FR-29): scoped, hashed at rest, revocable (SQLite dialect).

CREATE TABLE IF NOT EXISTS api_keys (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,   -- hex(SHA-256(full key))
    prefix       TEXT NOT NULL,          -- first chars of the key, for identification
    scope        TEXT NOT NULL,          -- 'admin' | 'affiliate:<id>'
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at TEXT,
    revoked_at   TEXT
);
