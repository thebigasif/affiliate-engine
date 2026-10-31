-- Postback macro contract (FR-7/FR-29): conversions must be traceable back
-- to the originating click and the merchant's order id, so the postback
-- emitter can substitute {click_id}/{order_id} instead of firing empty
-- macros. postback_secret enables HMAC-signed outbound postbacks (signed
-- webhooks out): generated when a postback URL is first set (admin PATCH);
-- empty = unsigned delivery (backward compatible).

ALTER TABLE commissions ADD COLUMN click_id TEXT;
ALTER TABLE commissions ADD COLUMN order_id TEXT;
CREATE INDEX IF NOT EXISTS ix_commissions_click ON commissions(click_id)
    WHERE click_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_commissions_order ON commissions(order_id)
    WHERE order_id IS NOT NULL;

ALTER TABLE affiliates ADD COLUMN postback_secret TEXT NOT NULL DEFAULT '';
