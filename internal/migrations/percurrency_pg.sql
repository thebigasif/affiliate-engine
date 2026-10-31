-- Per-currency money (PostgreSQL dialect; see percurrency_sqlite.sql for
-- the semantics). rehomeLedger (migrations.go) runs after this script.

ALTER TABLE ledger_accounts ADD COLUMN IF NOT EXISTS currency TEXT;

ALTER TABLE program_settings ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'USD';

ALTER TABLE commission_rules ADD COLUMN IF NOT EXISTS currency TEXT;
ALTER TABLE coupons ADD COLUMN IF NOT EXISTS currency TEXT;
ALTER TABLE rate_tiers ADD COLUMN IF NOT EXISTS currency TEXT;

CREATE TABLE IF NOT EXISTS payout_thresholds (
    currency   TEXT PRIMARY KEY,
    min_minor  BIGINT NOT NULL CHECK (min_minor >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

UPDATE commissions SET currency = 'USD' WHERE TRIM(currency) = '';
UPDATE commissions SET currency = UPPER(TRIM(currency)) WHERE currency <> UPPER(TRIM(currency));
UPDATE payouts SET currency = 'USD' WHERE TRIM(currency) = '';
UPDATE payouts SET currency = UPPER(TRIM(currency)) WHERE currency <> UPPER(TRIM(currency));
