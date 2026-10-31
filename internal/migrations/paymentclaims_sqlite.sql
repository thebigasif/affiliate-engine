-- One credit per payment. A Stripe payment-mode Checkout with
-- invoice_creation sends checkout.session.completed AND invoice.paid for
-- the same money. The credit transaction claims the payment's ids here
-- (commissions.Event.PaymentKeys), so a second event for the same payment
-- finds the claim and credits nothing, across processes and even when the
-- refund link rows were never written. commission_id lets a refund find
-- the commission through the claim.
CREATE TABLE IF NOT EXISTS payment_claims (
    source        TEXT NOT NULL,
    payment_key   TEXT NOT NULL,
    event_id      TEXT NOT NULL,
    commission_id INTEGER NOT NULL REFERENCES commissions(id),
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (source, payment_key)
);
CREATE INDEX IF NOT EXISTS ix_payment_claims_commission ON payment_claims(commission_id);
