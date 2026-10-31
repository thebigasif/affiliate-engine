-- MLM engine schema (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS mlm_closure (
    ancestor_id   BIGINT NOT NULL REFERENCES affiliates(id),
    descendant_id BIGINT NOT NULL REFERENCES affiliates(id),
    outer_leg     TEXT,
    level         INTEGER NOT NULL
);
-- See mlm_sqlite.sql: partial unique indexes instead of a composite PK —
-- a PK would force outer_leg NOT NULL and reject sponsor-tree rows
-- (outer_leg NULL) entirely on PG. ON CONFLICT DO NOTHING matches these.
CREATE UNIQUE INDEX IF NOT EXISTS ux_mlm_closure_sponsor
    ON mlm_closure(ancestor_id, descendant_id) WHERE outer_leg IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS ux_mlm_closure_placement
    ON mlm_closure(ancestor_id, descendant_id, outer_leg) WHERE outer_leg IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_mlm_closure_desc_sponsor
    ON mlm_closure(descendant_id) WHERE outer_leg IS NULL;
CREATE INDEX IF NOT EXISTS ix_mlm_closure_desc_placement
    ON mlm_closure(descendant_id) WHERE outer_leg IS NOT NULL;

CREATE TABLE IF NOT EXISTS mlm_volume_daily (
    day           INTEGER NOT NULL,
    affiliate_id  BIGINT NOT NULL REFERENCES affiliates(id),
    bv_personal   INTEGER NOT NULL DEFAULT 0,
    bv_group      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, affiliate_id)
);

CREATE TABLE IF NOT EXISTS mlm_ranks (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    required_bv   INTEGER NOT NULL,
    payout_factor INTEGER NOT NULL DEFAULT 10000
);

CREATE TABLE IF NOT EXISTS mlm_tier_rates (
    campaign_id INTEGER NOT NULL DEFAULT 1,
    level       INTEGER NOT NULL,
    rate_bp     INTEGER NOT NULL,
    PRIMARY KEY (campaign_id, level)
);

CREATE TABLE IF NOT EXISTS mlm_payout_periods (
    id           BIGSERIAL PRIMARY KEY,
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','calculating','paid','failed')),
    UNIQUE (period_start, period_end)
);

CREATE TABLE IF NOT EXISTS mlm_period_lines (
    period_id    BIGINT NOT NULL REFERENCES mlm_payout_periods(id),
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    level        INTEGER NOT NULL,
    commission_cents BIGINT NOT NULL,
    rate_bp_used     INTEGER NOT NULL,
    source_commission_id INTEGER NOT NULL,
    PRIMARY KEY (period_id, affiliate_id, level, source_commission_id)
);

CREATE TABLE IF NOT EXISTS pair_volume (
    affiliate_id  BIGINT NOT NULL REFERENCES affiliates(id),
    day           INTEGER NOT NULL,
    leg_l         BIGINT NOT NULL DEFAULT 0,
    leg_r         BIGINT NOT NULL DEFAULT 0,
    pairs_paid    BIGINT NOT NULL DEFAULT 0,
    payout_cents  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (affiliate_id, day)
);

-- F2 fix (see mlm_sqlite.sql): dedupe before the unique index — keep the
-- earliest period's row per (affiliate, level, source) entitlement. (No
-- rowid on PG and no MIN(tid) aggregate, hence the semi-join form.)
DELETE FROM mlm_period_lines AS pl
WHERE EXISTS (
    SELECT 1 FROM mlm_period_lines x
    WHERE x.affiliate_id = pl.affiliate_id
      AND x.level = pl.level
      AND x.source_commission_id = pl.source_commission_id
      AND x.period_id < pl.period_id
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_mlm_tier_source
    ON mlm_period_lines (affiliate_id, level, source_commission_id);

CREATE UNIQUE INDEX IF NOT EXISTS uq_mlm_tier_rate
    ON mlm_tier_rates (campaign_id, level);
