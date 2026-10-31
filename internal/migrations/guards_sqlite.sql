-- Guard triggers, re-asserted on EVERY migration run (SQLite dialect; PG
-- mirror in guards_pg.sql). Earlier scripts create triggers with
-- CREATE TRIGGER IF NOT EXISTS, which never replaces an existing
-- definition: a fix to a guard would never reach an existing install, and
-- a restored or tampered database file would keep a neutered trigger under
-- the right name forever. This script runs LAST in the chain and drops +
-- recreates every money and audit guard, so the definitions below are the
-- authoritative ones.

-- ---------- ledger: append-only ----------

-- A journal moves ONE currency: every line's account holds the same
-- currency (ledger_accounts.currency; NULL only on legacy accounts, which
-- the per-currency migration removes), so a journal summing to zero
-- balances in each currency.
DROP TRIGGER IF EXISTS trg_journal_balanced;
CREATE TRIGGER trg_journal_balanced
AFTER INSERT ON ledger_entries
BEGIN
    SELECT CASE
        WHEN (SELECT COUNT(*) FROM ledger_entries WHERE journal_id = NEW.journal_id) >= 2
         AND (SELECT COALESCE(SUM(amount),0) FROM ledger_entries WHERE journal_id = NEW.journal_id) != 0
        THEN RAISE(ABORT, 'journal_unbalanced')
        WHEN (SELECT COUNT(DISTINCT COALESCE(la.currency, ''))
              FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
              WHERE le.journal_id = NEW.journal_id) > 1
        THEN RAISE(ABORT, 'journal_mixed_currency')
    END;
END;

DROP TRIGGER IF EXISTS trg_ledger_entries_no_update;
CREATE TRIGGER trg_ledger_entries_no_update BEFORE UPDATE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;

DROP TRIGGER IF EXISTS trg_ledger_entries_no_delete;
CREATE TRIGGER trg_ledger_entries_no_delete BEFORE DELETE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;

-- Journals are append-only too: idem_key is the replay guard, so an
-- UPDATE that renames it would re-open a processed event for a second post.
DROP TRIGGER IF EXISTS trg_journal_no_update;
CREATE TRIGGER trg_journal_no_update BEFORE UPDATE ON journal_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;

DROP TRIGGER IF EXISTS trg_journal_no_delete;
CREATE TRIGGER trg_journal_no_delete BEFORE DELETE ON journal_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;

-- ---------- commissions ----------

DROP TRIGGER IF EXISTS trg_commission_status;
CREATE TRIGGER trg_commission_status
BEFORE UPDATE OF status ON commissions
WHEN OLD.status != NEW.status
BEGIN
    SELECT CASE
        WHEN (OLD.status = 'pending'  AND NEW.status IN ('approved','cancelled'))
          OR (OLD.status = 'approved' AND NEW.status IN ('paid','cancelled','clawback'))
          OR (OLD.status = 'paid'     AND NEW.status = 'clawback')
          -- MO-16: a reversed payout's commission is owed again, but only
          -- once its payout is reversed and it is in no payout any more.
          OR (OLD.status = 'paid' AND NEW.status = 'approved'
              AND EXISTS (SELECT 1 FROM payout_reversal_lines rl
                          JOIN payouts p ON p.id = rl.payout_id
                          WHERE rl.commission_id = NEW.id AND p.reversed_at IS NOT NULL)
              AND NOT EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = NEW.id))
        THEN 1
        ELSE RAISE(ABORT, 'illegal_status_transition:' || OLD.status || '->' || NEW.status)
    END;
END;

-- Currency is money too: it may only be re-spelled ('usd' -> 'USD',
-- '' -> 'USD', the per-currency migration's normalization), never changed.
DROP TRIGGER IF EXISTS trg_commission_money_immutable;
CREATE TRIGGER trg_commission_money_immutable
BEFORE UPDATE ON commissions
WHEN OLD.status != 'pending' OR NEW.amount != OLD.amount OR NEW.journal_id != OLD.journal_id
  OR (CASE WHEN TRIM(NEW.currency) = '' THEN 'USD' ELSE UPPER(TRIM(NEW.currency)) END) != (CASE WHEN TRIM(OLD.currency) = '' THEN 'USD' ELSE UPPER(TRIM(OLD.currency)) END)
BEGIN
    SELECT CASE
        WHEN NEW.amount != OLD.amount OR NEW.journal_id != OLD.journal_id
          OR (CASE WHEN TRIM(NEW.currency) = '' THEN 'USD' ELSE UPPER(TRIM(NEW.currency)) END) != (CASE WHEN TRIM(OLD.currency) = '' THEN 'USD' ELSE UPPER(TRIM(OLD.currency)) END)
        THEN RAISE(ABORT, 'commission_immutable')
    END;
END;

-- A commission backed by booked money (its journal has ledger lines) can
-- never be deleted: the row is the domain fact behind the journal. No
-- production path deletes commissions; cancellation is a status change.
DROP TRIGGER IF EXISTS trg_commission_no_delete;
CREATE TRIGGER trg_commission_no_delete BEFORE DELETE ON commissions
WHEN EXISTS (SELECT 1 FROM ledger_entries WHERE journal_id = OLD.journal_id)
BEGIN SELECT RAISE(ABORT, 'commission_immutable'); END;

-- ---------- payouts ----------

DROP TRIGGER IF EXISTS trg_payout_money_immutable;
CREATE TRIGGER trg_payout_money_immutable BEFORE UPDATE ON payouts
WHEN NEW.amount != OLD.amount OR NEW.affiliate_id != OLD.affiliate_id
  OR (CASE WHEN TRIM(NEW.currency) = '' THEN 'USD' ELSE UPPER(TRIM(NEW.currency)) END) != (CASE WHEN TRIM(OLD.currency) = '' THEN 'USD' ELSE UPPER(TRIM(OLD.currency)) END)
BEGIN SELECT RAISE(ABORT, 'payout_immutable'); END;

-- Status machine, exactly the transitions payouts.Runner makes:
--   requested -> approved | processing   (Approve CAS)
--   approved  -> processing
--   processing -> requested              (hold / tax-doc / gate / rail failure: retry)
--   processing -> approved               (a Mark Paid settle that failed: no money moved)
--   processing -> paid | failed
--   paid -> failed only with reversed_at set (ReversePaid, MO-16)
-- paid and failed are otherwise terminal (failed = a real rail may have moved money;
-- re-opening it would allow a second transfer).
DROP TRIGGER IF EXISTS trg_payout_status;
CREATE TRIGGER trg_payout_status
BEFORE UPDATE OF status ON payouts
WHEN OLD.status != NEW.status
BEGIN
    SELECT CASE
        WHEN (OLD.status = 'requested'  AND NEW.status IN ('approved','processing'))
          OR (OLD.status = 'approved'   AND NEW.status = 'processing')
          OR (OLD.status = 'processing' AND NEW.status IN ('requested','approved','paid','failed'))
          OR (OLD.status = 'paid' AND NEW.status = 'failed' AND NEW.reversed_at IS NOT NULL)
        THEN 1
        ELSE RAISE(ABORT, 'illegal_payout_transition')
    END;
END;

DROP TRIGGER IF EXISTS trg_payout_no_delete;
CREATE TRIGGER trg_payout_no_delete BEFORE DELETE ON payouts
BEGIN SELECT RAISE(ABORT, 'payout_immutable'); END;

-- payout_lines: the ONLY legitimate delete is Runner.Approve releasing a
-- payout it holds in 'processing' (under the payout + commission money
-- locks) before any rail executes: a member commission left 'approved', or
-- a clawback landed after staging, so the lines return to the pool and the
-- next batch re-stages them at the correct net amount. Lines of a payout
-- in any other state (requested, paid, failed) are immutable, except a
-- reversed payout's (ReversePaid copies them to payout_reversal_lines
-- first and releases them so its commissions can be paid again).
DROP TRIGGER IF EXISTS trg_payout_lines_no_delete;
CREATE TRIGGER trg_payout_lines_no_delete BEFORE DELETE ON payout_lines
WHEN NOT EXISTS (SELECT 1 FROM payouts p WHERE p.id = OLD.payout_id
                 AND (p.status = 'processing' OR (p.status = 'failed' AND p.reversed_at IS NOT NULL
                      AND EXISTS (SELECT 1 FROM payout_reversal_lines rl
                                  WHERE rl.payout_id = OLD.payout_id AND rl.commission_id = OLD.commission_id))))
BEGIN SELECT RAISE(ABORT, 'payout_line_immutable'); END;

-- Export claim is write-once: a payout listed in a PayPal/Wise file keeps
-- its exported_at and export_batch forever, so no path can clear the
-- claim and list it in a second file.
DROP TRIGGER IF EXISTS trg_payout_export_once;
CREATE TRIGGER trg_payout_export_once
BEFORE UPDATE OF exported_at, export_batch ON payouts
WHEN OLD.exported_at IS NOT NULL
 AND (NEW.exported_at IS NULL OR NEW.exported_at != OLD.exported_at
      OR NEW.export_batch IS NULL OR NEW.export_batch != OLD.export_batch)
BEGIN SELECT RAISE(ABORT, 'payout_export_immutable'); END;

-- ---------- audit log ----------

DROP TRIGGER IF EXISTS trg_audit_no_update;
CREATE TRIGGER trg_audit_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;

DROP TRIGGER IF EXISTS trg_audit_no_delete;
CREATE TRIGGER trg_audit_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;

DROP TRIGGER IF EXISTS trg_audit_commission_status;
CREATE TRIGGER trg_audit_commission_status
AFTER UPDATE OF status ON commissions
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('commissions', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;

DROP TRIGGER IF EXISTS trg_audit_payout_status;
CREATE TRIGGER trg_audit_payout_status
AFTER UPDATE OF status ON payouts
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('payouts', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;

DROP TRIGGER IF EXISTS trg_audit_affiliate_status;
CREATE TRIGGER trg_audit_affiliate_status
AFTER UPDATE OF status ON affiliates
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('affiliates', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;

DROP TRIGGER IF EXISTS trg_audit_taxdoc_status;
CREATE TRIGGER trg_audit_taxdoc_status
AFTER UPDATE OF status ON tax_documents
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('tax_documents', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;
