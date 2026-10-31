-- Per-currency aggregates (PostgreSQL dialect; see aggcurrency_sqlite.sql).

ALTER TABLE mlm_period_lines ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT '';
UPDATE mlm_period_lines
SET currency = COALESCE((SELECT c.currency FROM commissions c WHERE c.id = mlm_period_lines.source_commission_id), 'USD')
WHERE currency = '';
