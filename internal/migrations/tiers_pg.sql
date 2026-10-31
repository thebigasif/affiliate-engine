-- FR-12 tiered rates: volume-based performance tiers (PG dialect; SQLite
-- mirror in tiers_sqlite.sql). See that file for the semantics contract.

CREATE TABLE IF NOT EXISTS rate_tiers (
    id              BIGSERIAL PRIMARY KEY,
    campaign_id     BIGINT NOT NULL DEFAULT 1,   -- no FK: campaign 1 is the implicit default (commission_rules convention)
    product_key     TEXT,
    threshold_minor BIGINT NOT NULL CHECK (threshold_minor >= 0),
    rate_type       TEXT NOT NULL CHECK (rate_type IN ('percent','fixed')),
    rate_value      BIGINT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_rate_tiers_campaign ON rate_tiers(campaign_id, active, threshold_minor);

ALTER TABLE commissions ADD COLUMN IF NOT EXISTS gross_minor BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS ix_commissions_volume
    ON commissions(affiliate_id, campaign_id) WHERE gross_minor > 0;
