-- Usage/metered billing (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS usage_events (
    id             BIGSERIAL PRIMARY KEY,
    source         TEXT NOT NULL,
    external_id    TEXT NOT NULL,
    customer_ext_id TEXT NOT NULL,
    units          BIGINT NOT NULL CHECK (units >= 0),
    unit_type      TEXT NOT NULL DEFAULT 'tokens',
    occurred_at    TIMESTAMPTZ NOT NULL,
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS ix_usage_customer ON usage_events(customer_ext_id, occurred_at);

CREATE TABLE IF NOT EXISTS usage_daily (
    day            INTEGER NOT NULL,
    customer_ext_id TEXT NOT NULL,
    unit_type      TEXT NOT NULL,
    units          BIGINT NOT NULL DEFAULT 0,
    settled_cents  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, customer_ext_id, unit_type)
);

-- Heal (2026-09): usage-settled commissions were inserted with
-- matures_at = NULL. Mature() selects `matures_at <= now`; NULL never
-- compares true on either engine, so every usage commission was stuck
-- 'pending' forever with its money stranded in HOLD (FR-16). Give
-- existing pending rows a NET-terms deadline measured from settlement
-- time. Idempotent — only touches still-NULL rows. (SQLite matches the
-- raw TEXT snapshot with LIKE; PG's JSONB renders with spaces after
-- colons, so use the ->> operator there instead.)
UPDATE commissions
SET matures_at = created_at + make_interval(days => COALESCE(
    (SELECT net_terms_days FROM campaigns WHERE id = 1), 30))
WHERE matures_at IS NULL
  AND status = 'pending'
  AND rate_snapshot->>'origin' = 'usage';
