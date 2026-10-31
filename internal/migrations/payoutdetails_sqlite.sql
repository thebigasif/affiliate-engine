-- Where each partner wants to be paid, entered in their portal. The
-- details (PayPal or Wise email, bank details) are sealed with AES-256-GCM
-- under a key derived from the deployment secret, like tax forms: a leaked
-- DB file or backup alone does not expose bank details. Only the method
-- is plain, for display.
CREATE TABLE IF NOT EXISTS affiliate_payout_details (
    affiliate_id INTEGER PRIMARY KEY REFERENCES affiliates(id),
    method       TEXT NOT NULL CHECK (method IN ('paypal','wise','bank')),
    sealed       BLOB NOT NULL,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
