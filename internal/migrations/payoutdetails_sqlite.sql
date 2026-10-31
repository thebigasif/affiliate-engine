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

-- A change to existing details holds the partner's payouts until this
-- time (RFC3339), so a stolen session or key cannot redirect money at
-- once. NULL: not cooling (a first-ever setup never cools).
ALTER TABLE affiliate_payout_details ADD COLUMN cooling_until TEXT;
