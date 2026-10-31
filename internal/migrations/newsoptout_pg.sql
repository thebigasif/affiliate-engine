-- Program news opt-out. PG mirror of newsoptout_sqlite.sql.
ALTER TABLE affiliates ADD COLUMN IF NOT EXISTS news_opt_out INTEGER NOT NULL DEFAULT 0;
