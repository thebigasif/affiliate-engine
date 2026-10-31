-- One credit per payment. PG mirror of paymentclaims_sqlite.sql.
CREATE TABLE IF NOT EXISTS payment_claims (
    source        TEXT NOT NULL,
    payment_key   TEXT NOT NULL,
    event_id      TEXT NOT NULL,
    commission_id BIGINT NOT NULL REFERENCES commissions(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, payment_key)
);
CREATE INDEX IF NOT EXISTS ix_payment_claims_commission ON payment_claims(commission_id);
