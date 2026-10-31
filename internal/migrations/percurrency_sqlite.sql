-- Per-currency money (SQLite dialect; PG mirror in percurrency_pg.sql).
--
-- Money is never summed or netted across currencies. Ledger accounts are
-- per currency (HOLD:USD, AFFILIATE_PAYABLE:<id>:JPY, ...; see
-- internal/ledger/accounts.go) and carry their currency in a column the
-- journal guard reads. After this script, rehomeLedger (migrations.go)
-- moves every legacy, currency-less account's lines to the per-currency
-- account of the journal's currency, in one transaction.

ALTER TABLE ledger_accounts ADD COLUMN currency TEXT;

-- The program currency: what a commission rule, coupon or volume tier
-- without an explicit currency is denominated in, and the currency
-- min_payout_cents is set in. Set on upgrade to the one currency an
-- install has booked commissions in (rehomeLedger), else USD.
ALTER TABLE program_settings ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';

-- Fixed amounts and volume thresholds are money: they carry a currency
-- (NULL = the program currency). A sale only matches a fixed rule or a
-- tier in its own currency; percent rules match every currency.
ALTER TABLE commission_rules ADD COLUMN currency TEXT;
ALTER TABLE coupons ADD COLUMN currency TEXT;
ALTER TABLE rate_tiers ADD COLUMN currency TEXT;

-- Per-currency minimum payout (minor units of that currency). A currency
-- without a row uses program_settings.min_payout_cents as it always has.
CREATE TABLE IF NOT EXISTS payout_thresholds (
    currency   TEXT PRIMARY KEY,
    min_minor  INTEGER NOT NULL CHECK (min_minor >= 0),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Currency codes are upper-case, and a row without one is in USD (the
-- column default; an adapter could store ''). Idempotent: only touches
-- rows that need it. Runs before guards_sqlite.sql makes currency
-- immutable on commissions and payouts.
UPDATE commissions SET currency = 'USD' WHERE TRIM(currency) = '';
UPDATE commissions SET currency = UPPER(TRIM(currency)) WHERE currency != UPPER(TRIM(currency));
UPDATE payouts SET currency = 'USD' WHERE TRIM(currency) = '';
UPDATE payouts SET currency = UPPER(TRIM(currency)) WHERE currency != UPPER(TRIM(currency));
