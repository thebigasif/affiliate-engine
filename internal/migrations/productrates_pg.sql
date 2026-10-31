-- FR-12: per-product/plan commission rates. product_key NULL = any product
-- (the pre-existing semantics); a non-NULL product_key only matches events
-- carrying that exact key. Specificity ordering is resolved in the engine's
-- resolveRule (coupon > affiliate+product > affiliate > campaign+product >
-- campaign default), so no schema constraint change is needed beyond the
-- column + lookup index.

ALTER TABLE commission_rules ADD COLUMN IF NOT EXISTS product_key TEXT;
CREATE INDEX IF NOT EXISTS ix_commission_rules_product
    ON commission_rules(campaign_id, product_key);
