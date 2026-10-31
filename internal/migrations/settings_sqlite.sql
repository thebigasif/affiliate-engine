-- Program payout settings (singleton row, id = 1).
--   min_payout_cents:  a payout run skips an (affiliate, currency) group
--                      whose net total is below this (minor units of the
--                      payout currency). Default 5000 ($50).
--   default_hold_days: hold (NET terms) for programs created without an
--                      explicit net_terms_days, and the maturity fallback
--                      when a commission's program row cannot be read.
--                      Each program's own campaigns.net_terms_days wins.
CREATE TABLE IF NOT EXISTS program_settings (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    min_payout_cents   INTEGER NOT NULL DEFAULT 5000
                           CHECK (min_payout_cents >= 0),
    default_hold_days  INTEGER NOT NULL DEFAULT 30
                           CHECK (default_hold_days BETWEEN 0 AND 365),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO program_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
