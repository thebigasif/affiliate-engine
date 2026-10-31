-- Core ledger accounts. Earlier releases seeded currency-less HOLD /
-- EXPENSE:COMMISSION / PAYOUT_CLEARING here and created
-- AFFILIATE_PAYABLE:<id> with every affiliate row (trg_affiliates_payable).
-- Accounts are per currency now (HOLD:USD, AFFILIATE_PAYABLE:<id>:JPY,
-- ...) and the ledger creates them on first use (internal/ledger
-- accounts.go), so nothing is seeded. The per-currency migration
-- (percurrency_pg.sql + rehomeLedger) re-homes the legacy accounts'
-- balances and removes them.

DROP TRIGGER IF EXISTS trg_affiliates_payable ON affiliates;
DROP FUNCTION IF EXISTS trg_affiliate_payable();
