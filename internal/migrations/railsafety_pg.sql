-- Money-rail safety. PG mirror of railsafety_sqlite.sql.
CREATE TABLE IF NOT EXISTS install_identity (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    install_id TEXT NOT NULL
);
INSERT INTO install_identity (id, install_id)
VALUES (1, substr(md5(random()::text || clock_timestamp()::text), 1, 16))
ON CONFLICT (id) DO NOTHING;

ALTER TABLE payouts ADD COLUMN IF NOT EXISTS rail_attempted_at TIMESTAMPTZ;
