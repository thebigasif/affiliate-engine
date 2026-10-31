-- Usage/metered billing (the AI-SaaS wedge): token/credit consumption
-- events that attribute recurring micro-commissions (SQLite dialect).

CREATE TABLE IF NOT EXISTS usage_events (
    id             INTEGER PRIMARY KEY,
    source         TEXT NOT NULL,              -- "api", "stripe_usage", ...
    external_id    TEXT NOT NULL,              -- idempotency (source event id)
    customer_ext_id TEXT NOT NULL,
    units          INTEGER NOT NULL CHECK (units >= 0), -- tokens/credits/seconds
    unit_type      TEXT NOT NULL DEFAULT 'tokens',
    occurred_at    TEXT NOT NULL,
    ingested_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS ix_usage_customer ON usage_events(customer_ext_id, occurred_at);

-- Aggregated meter per (customer, day, unit_type) — the rollup the
-- commission engine reads (avoids per-event commission rows at token scale).
CREATE TABLE IF NOT EXISTS usage_daily (
    day            INTEGER NOT NULL,
    customer_ext_id TEXT NOT NULL,
    unit_type      TEXT NOT NULL,
    units          INTEGER NOT NULL DEFAULT 0,
    settled_cents  INTEGER NOT NULL DEFAULT 0,  -- cumulative settled commission
    PRIMARY KEY (day, customer_ext_id, unit_type)
);

-- Heal (2026-09): usage-settled commissions were inserted with
-- matures_at = NULL. Mature() selects `matures_at <= now`; NULL never
-- compares true on either engine, so every usage commission was stuck
-- 'pending' forever with its money stranded in HOLD (FR-16). Give
-- existing pending rows a NET-terms deadline measured from settlement
-- time. Idempotent — only touches still-NULL rows.
UPDATE commissions
SET matures_at = strftime('%Y-%m-%dT%H:%M:%SZ', created_at,
    '+' || COALESCE((SELECT net_terms_days FROM campaigns WHERE id = 1), 30) || ' days')
WHERE matures_at IS NULL
  AND status = 'pending'
  AND rate_snapshot LIKE '{"origin":"usage"%';
