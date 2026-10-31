-- Program payout settings (singleton row, id = 1). PG mirror of
-- settings_sqlite.sql; see that file for the semantics.
CREATE TABLE IF NOT EXISTS program_settings (
    id                 BIGINT PRIMARY KEY CHECK (id = 1),
    min_payout_cents   BIGINT NOT NULL DEFAULT 5000
                           CHECK (min_payout_cents >= 0),
    default_hold_days  INTEGER NOT NULL DEFAULT 30
                           CHECK (default_hold_days BETWEEN 0 AND 365),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO program_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
