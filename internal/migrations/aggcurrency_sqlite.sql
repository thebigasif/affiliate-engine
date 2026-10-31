-- Per-currency aggregates (SQLite dialect; PG mirror in aggcurrency_pg.sql).
--
-- daily_agg and token_stats_daily get a currency in their key (see
-- clicks_sqlite.sql and tokenstats_sqlite.sql for the shape). A database
-- written before that is rebuilt by aggregatesPerCurrency (aggcurrency.go),
-- which runs right after this script.

-- An MLM tier split is in the currency of the commission it splits.
-- Backfilled from that commission; idempotent (only rows without one).
ALTER TABLE mlm_period_lines ADD COLUMN currency TEXT NOT NULL DEFAULT '';
UPDATE mlm_period_lines
SET currency = COALESCE((SELECT c.currency FROM commissions c WHERE c.id = mlm_period_lines.source_commission_id), 'USD')
WHERE currency = '';
