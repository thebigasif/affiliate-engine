-- MLM engine schema (SQLite dialect; PG mirror in mlm_pg.sql).
-- Design: PLAN.md §3.5 (guruperl-derived logic, clean-room).
-- Two explicit trees: sponsor (unilevel) and placement (binary). Never conflated.

-- Sponsor/placement parents live on affiliates (nullable = root/no tree).
-- mlm_closure: the precomputed ancestor path with outer-leg tagging.

CREATE TABLE IF NOT EXISTS mlm_closure (
    ancestor_id   INTEGER NOT NULL REFERENCES affiliates(id),
    descendant_id INTEGER NOT NULL REFERENCES affiliates(id),
    outer_leg     TEXT,                -- NULL=sponsor tree; 'L'/'R' = placement outer leg
    level         INTEGER NOT NULL     -- 1 = direct parent
);
-- Dedup keys. NULL outer_leg must participate in uniqueness for sponsor
-- rows: SQLite treats NULLs as distinct in PRIMARY KEY/unique indexes, and
-- PG PRIMARY KEY forces outer_leg NOT NULL (rejecting sponsor rows
-- outright). Two partial unique indexes give NULL-intolerant uniqueness
-- per tree on BOTH engines, and heal existing installs (index-only
-- migration; the bare-table PRIMARY KEY above is deliberately gone).
CREATE UNIQUE INDEX IF NOT EXISTS ux_mlm_closure_sponsor
    ON mlm_closure(ancestor_id, descendant_id) WHERE outer_leg IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS ux_mlm_closure_placement
    ON mlm_closure(ancestor_id, descendant_id, outer_leg) WHERE outer_leg IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_mlm_closure_desc_sponsor
    ON mlm_closure(descendant_id) WHERE outer_leg IS NULL;
CREATE INDEX IF NOT EXISTS ix_mlm_closure_desc_placement
    ON mlm_closure(descendant_id) WHERE outer_leg IS NOT NULL;

-- Volume counters (on-write, class 2-3 BV from the study; recompute job self-heals).
CREATE TABLE IF NOT EXISTS mlm_volume_daily (
    day           INTEGER NOT NULL,
    affiliate_id  INTEGER NOT NULL REFERENCES affiliates(id),
    bv_personal   INTEGER NOT NULL DEFAULT 0,  -- own sales volume (BV points)
    bv_group      INTEGER NOT NULL DEFAULT 0,  -- downline volume (sponsor tree)
    PRIMARY KEY (day, affiliate_id)
);

-- Rank definitions (checked-in program config; seedable).
CREATE TABLE IF NOT EXISTS mlm_ranks (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    required_bv   INTEGER NOT NULL,          -- cumulative group BV threshold
    payout_factor INTEGER NOT NULL DEFAULT 10000  -- basis points of bonus pool (10000 = 100%)
);

-- Commission plan: per-tier rates for multi-tier splits.
CREATE TABLE IF NOT EXISTS mlm_tier_rates (
    campaign_id INTEGER NOT NULL DEFAULT 1,
    level       INTEGER NOT NULL,            -- 1 = direct sponsor, 2 = grand-sponsor...
    rate_bp     INTEGER NOT NULL,            -- basis points of commissionable amount
    PRIMARY KEY (campaign_id, level)
);

-- The payout period calendar (CAS-flagged runs).
CREATE TABLE IF NOT EXISTS mlm_payout_periods (
    id           INTEGER PRIMARY KEY,
    period_start TEXT NOT NULL,
    period_end   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','calculating','paid','failed')),
    UNIQUE (period_start, period_end)
);

-- Period results (idempotent insert: unique period x affiliate x level).
CREATE TABLE IF NOT EXISTS mlm_period_lines (
    period_id    INTEGER NOT NULL REFERENCES mlm_payout_periods(id),
    affiliate_id INTEGER NOT NULL REFERENCES affiliates(id),
    level        INTEGER NOT NULL,
    commission_cents INTEGER NOT NULL,
    rate_bp_used     INTEGER NOT NULL,
    source_commission_id INTEGER NOT NULL,
    PRIMARY KEY (period_id, affiliate_id, level, source_commission_id)
);

-- Binary pairing state (clean-room from the guruperl study; no float,
-- no silently-consumed BV: weaker-leg deduction is explicit in pairs_paid).
CREATE TABLE IF NOT EXISTS pair_volume (
    affiliate_id  INTEGER NOT NULL REFERENCES affiliates(id),
    day           INTEGER NOT NULL,
    leg_l         INTEGER NOT NULL DEFAULT 0,
    leg_r         INTEGER NOT NULL DEFAULT 0,
    pairs_paid    INTEGER NOT NULL DEFAULT 0,
    payout_cents  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (affiliate_id, day)
);

-- F2 fix: a source commission can pay each (affiliate, level) ONCE, ever —
-- regardless of which period window picks it up. Overlapping/corrected
-- windows must not re-pay. Heal pre-existing duplicates FIRST (keep the
-- earliest period's row), or the unique index below would brick upgrades.
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

-- A (campaign, level) has exactly one rate; duplicates would make tier
-- payouts nondeterministic/multiplied.
CREATE UNIQUE INDEX IF NOT EXISTS uq_mlm_tier_rate
    ON mlm_tier_rates (campaign_id, level);
