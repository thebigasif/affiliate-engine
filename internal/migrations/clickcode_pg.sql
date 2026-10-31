-- The referral code a click came through. PG mirror of clickcode_sqlite.sql.
ALTER TABLE clicks ADD COLUMN IF NOT EXISTS ref_code TEXT NOT NULL DEFAULT '';
