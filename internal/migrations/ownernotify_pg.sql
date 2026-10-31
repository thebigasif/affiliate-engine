-- Owner notification email. PG mirror of ownernotify_sqlite.sql.
CREATE TABLE IF NOT EXISTS owner_notify (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    email        TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_sent_at TEXT,
    last_key     TEXT NOT NULL DEFAULT ''
);
