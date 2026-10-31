-- Partner payout details. PG mirror of payoutdetails_sqlite.sql.
CREATE TABLE IF NOT EXISTS affiliate_payout_details (
    affiliate_id BIGINT PRIMARY KEY REFERENCES affiliates(id),
    method       TEXT NOT NULL CHECK (method IN ('paypal','wise','bank')),
    sealed       BYTEA NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Cooling period after a change. See payoutdetails_sqlite.sql.
ALTER TABLE affiliate_payout_details ADD COLUMN IF NOT EXISTS cooling_until TIMESTAMPTZ;
