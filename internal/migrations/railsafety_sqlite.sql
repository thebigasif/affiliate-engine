-- Money-rail safety.
--
-- install_identity: a random id minted once per database. Stripe Connect
-- transfer groups and idempotency keys carry it, so two installs sharing
-- a Stripe key (staging and production), or one database reset, never
-- collide on payout ids.
CREATE TABLE IF NOT EXISTS install_identity (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    install_id TEXT NOT NULL
);
INSERT INTO install_identity (id, install_id) VALUES (1, lower(hex(randomblob(8))))
ON CONFLICT (id) DO NOTHING;

-- rail_attempted_at: when Approve first handed the payout to a real money
-- rail. A payout whose rail was attempted is never restaged before the
-- rail is asked whether the transfer landed.
ALTER TABLE payouts ADD COLUMN rail_attempted_at TEXT;
