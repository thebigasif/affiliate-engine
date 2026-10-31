-- The program a customer was bound under. PG mirror of custcampaign_sqlite.sql.
ALTER TABLE customers ADD COLUMN IF NOT EXISTS campaign_id BIGINT NOT NULL DEFAULT 1;
