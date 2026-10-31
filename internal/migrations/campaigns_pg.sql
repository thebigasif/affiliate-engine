-- Campaigns / programs (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS campaigns (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','paused','archived')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-program configuration (idempotent; defaults preserve legacy behavior).
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS attribution_window_days INTEGER NOT NULL DEFAULT 30;
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS net_terms_days          INTEGER NOT NULL DEFAULT 30;
-- How many months a customer earns its affiliate commission (0 = lifetime).
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS commission_months INTEGER NOT NULL DEFAULT 0;
