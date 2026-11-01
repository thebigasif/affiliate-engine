-- Fraud ruleset completion: columns the four new rules + the coupon-hijack
-- rewrite need (SQLite dialect; pg mirror in fraud2_pg.sql).
-- Applied idempotently — re-runs trip the duplicate-column convention.

-- Attribution provenance on every commission row (rules read these instead
-- of grepping raw_events payloads):
ALTER TABLE commissions ADD COLUMN attribution_method TEXT NOT NULL DEFAULT '';
ALTER TABLE commissions ADD COLUMN coupon_code        TEXT NOT NULL DEFAULT '';
ALTER TABLE commissions ADD COLUMN customer_ext_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE commissions ADD COLUMN country            TEXT NOT NULL DEFAULT '';

-- Click geo (FR-2 geo parse): ISO-3166 alpha-2 captured at the redirect
-- edge from the CDN geo header; no raw location data stored.
ALTER TABLE clicks ADD COLUMN country TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS ix_commissions_aff_created ON commissions(affiliate_id, created_at);
CREATE INDEX IF NOT EXISTS ix_commissions_coupon ON commissions(coupon_code, created_at)
    WHERE coupon_code != '';

-- screened_at: when screening committed for the commission, in the same
-- transaction as its credit. NULL = never screened (credited by a release
-- that screened after the credit committed, or not through screening); a
-- replayed delivery screens it then.
ALTER TABLE commissions ADD COLUMN screened_at TEXT;

-- hold_group: holds placed together by one partner-wide verdict (a rule
-- about the affiliate's overall behaviour) share a group, so the owner
-- reviews them together and releasing one releases the rest. NULL = a
-- hold on its own.
ALTER TABLE commission_holds ADD COLUMN hold_group TEXT;
CREATE INDEX IF NOT EXISTS ix_commission_holds_group ON commission_holds(hold_group)
    WHERE hold_group IS NOT NULL AND resolved_at IS NULL;
