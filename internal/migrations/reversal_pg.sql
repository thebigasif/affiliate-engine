-- Paid-payout reversal (FR-119 revert, MO-16). PG mirror of
-- reversal_sqlite.sql.
ALTER TABLE payouts ADD COLUMN IF NOT EXISTS reversed_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS payout_reversal_lines (
    payout_id     BIGINT NOT NULL REFERENCES payouts(id),
    commission_id BIGINT NOT NULL REFERENCES commissions(id),
    PRIMARY KEY (payout_id, commission_id)
);
CREATE INDEX IF NOT EXISTS ix_payout_reversal_lines_commission ON payout_reversal_lines(commission_id);
