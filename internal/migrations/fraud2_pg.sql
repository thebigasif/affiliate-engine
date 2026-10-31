-- Fraud ruleset completion: columns the four new rules + the coupon-hijack
-- rewrite need (PostgreSQL dialect; sqlite mirror in fraud2_sqlite.sql).

ALTER TABLE commissions ADD COLUMN IF NOT EXISTS attribution_method TEXT NOT NULL DEFAULT '';
ALTER TABLE commissions ADD COLUMN IF NOT EXISTS coupon_code        TEXT NOT NULL DEFAULT '';
ALTER TABLE commissions ADD COLUMN IF NOT EXISTS customer_ext_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE commissions ADD COLUMN IF NOT EXISTS country            TEXT NOT NULL DEFAULT '';

ALTER TABLE clicks ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS ix_commissions_aff_created ON commissions(affiliate_id, created_at);
CREATE INDEX IF NOT EXISTS ix_commissions_coupon ON commissions(coupon_code, created_at)
    WHERE coupon_code != '';

ALTER TABLE commissions ADD COLUMN IF NOT EXISTS screened_at TIMESTAMPTZ;
