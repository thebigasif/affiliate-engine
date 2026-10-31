-- Manual-rail export claim. PG mirror of payoutexport_sqlite.sql.
ALTER TABLE payouts ADD COLUMN IF NOT EXISTS exported_at TIMESTAMPTZ;
ALTER TABLE payouts ADD COLUMN IF NOT EXISTS export_batch TEXT;
ALTER TABLE payouts ADD COLUMN IF NOT EXISTS attention TEXT;
