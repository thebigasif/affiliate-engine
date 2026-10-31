-- Manual-rail export claim (PayPal / Wise CSV). A payout listed in a CSV
-- file is claimed: exported_at + export_batch are stamped under the
-- payout's money lock, so the same payout can never appear in a second
-- file, and Approve never restages it into a new run (the file may
-- already have been paid). attention holds the reason an exported payout
-- could not settle (hold, tax doc, commission left 'approved', clawback
-- after export); NULL when nothing needs the operator. Status is
-- unchanged ('requested'), so no CHECK constraint or status-machine edit.
ALTER TABLE payouts ADD COLUMN exported_at TEXT;
ALTER TABLE payouts ADD COLUMN export_batch TEXT;
ALTER TABLE payouts ADD COLUMN attention TEXT;
