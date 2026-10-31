-- Campaigns / programs (SQLite dialect).

CREATE TABLE IF NOT EXISTS campaigns (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','paused','archived')),
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Per-program configuration (idempotent; defaults preserve legacy behavior).
ALTER TABLE campaigns ADD COLUMN attribution_window_days INTEGER NOT NULL DEFAULT 30;
ALTER TABLE campaigns ADD COLUMN net_terms_days          INTEGER NOT NULL DEFAULT 30;
-- How many months a customer earns its affiliate commission (0 = lifetime).
ALTER TABLE campaigns ADD COLUMN commission_months INTEGER NOT NULL DEFAULT 0;
