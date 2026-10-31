-- Paid-payout reversal (FR-119 revert, MO-16).
--
-- A payout reversed after it was paid ends 'failed' with reversed_at set
-- (the sanctioned reversed state: the status machine allows paid ->
-- failed only with reversed_at). Its lines are moved to
-- payout_reversal_lines (history) and released, so its commissions can
-- return from 'paid' to 'approved' and be paid again; the ledger gets the
-- reversing journal (CLEARING -> PAYABLE). guards_sqlite.sql holds the
-- trigger rules.
ALTER TABLE payouts ADD COLUMN reversed_at TEXT;

CREATE TABLE IF NOT EXISTS payout_reversal_lines (
    payout_id     INTEGER NOT NULL REFERENCES payouts(id),
    commission_id INTEGER NOT NULL REFERENCES commissions(id),
    PRIMARY KEY (payout_id, commission_id)
);
CREATE INDEX IF NOT EXISTS ix_payout_reversal_lines_commission ON payout_reversal_lines(commission_id);
