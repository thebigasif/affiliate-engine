-- The program owner's "something needs you" email: where it goes, whether
-- it's on, and what the last one covered (so a pile that hasn't changed
-- isn't sent again). One row; absent = on, to AE_OWNER_EMAIL.
CREATE TABLE IF NOT EXISTS owner_notify (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    email        TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_sent_at TEXT,
    last_key     TEXT NOT NULL DEFAULT ''
);
