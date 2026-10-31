-- FR-12 tiered rates: volume-based performance tiers (SQLite dialect; PG
-- mirror in tiers_pg.sql). A tier ladder is a set of rows per campaign;
-- each row's threshold_minor is cumulative referred revenue (gross) at
-- which the tier's rate replaces the campaign-level base rate.
--
-- Semantics (resolved in commissions.resolveRule):
--   coupon > affiliate+product > affiliate > campaign+product >
--   crossed volume tier (product-specific, then agnostic) > campaign default
-- Tiers never supersede negotiated overrides (coupon / affiliate rules) nor
-- deliberate product economics (campaign+product) — a crossed tier replaces
-- only the BASE rate. Among crossed tiers the highest threshold wins,
-- product-specific over product-agnostic. Volume basis = SUM(gross_minor)
-- of the affiliate's non-cancelled, non-clawed-back commissions in the
-- campaign (measured BEFORE the current event lands).

CREATE TABLE IF NOT EXISTS rate_tiers (
    id              INTEGER PRIMARY KEY,
    campaign_id     INTEGER NOT NULL DEFAULT 1,  -- no FK: campaign 1 is the implicit default (commission_rules convention)
    product_key     TEXT,                        -- NULL = any product
    threshold_minor INTEGER NOT NULL CHECK (threshold_minor >= 0),
    rate_type       TEXT NOT NULL CHECK (rate_type IN ('percent','fixed')),
    rate_value      INTEGER NOT NULL,            -- percent: basis points; fixed: minor units
    active          INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS ix_rate_tiers_campaign ON rate_tiers(campaign_id, active, threshold_minor);

-- The volume basis: every credited commission snapshots its gross
-- (commissionable) amount. Clawback rows keep the 0 default (they are not
-- sales). Legacy rows read as 0 — ladders start measuring at deployment.
ALTER TABLE commissions ADD COLUMN gross_minor INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS ix_commissions_volume
    ON commissions(affiliate_id, campaign_id) WHERE gross_minor > 0;
