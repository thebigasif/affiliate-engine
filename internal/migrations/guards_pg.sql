-- Guard triggers (PostgreSQL dialect; SQLite mirror in guards_sqlite.sql).
-- Runs LAST in the chain on every Apply. The PG scripts already use the
-- DROP-then-CREATE pattern, so this file adds only what the earlier
-- scripts lack: no-UPDATE on journals, no-DELETE on commissions / payouts
-- / payout_lines, the payout status machine, and TRUNCATE guards (row-level
-- BEFORE DELETE/UPDATE triggers never fire on TRUNCATE).

CREATE OR REPLACE FUNCTION forbid() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION '%', TG_ARGV[0]; END; $$ LANGUAGE plpgsql;

-- ---------- journals: balanced, one currency (see guards_sqlite.sql) ----------
-- Replaces pg.sql's definition, which only checked the sum.

CREATE OR REPLACE FUNCTION trg_journal_balanced() RETURNS trigger AS $$
DECLARE
    n_lines BIGINT; s BIGINT; n_cur BIGINT;
BEGIN
    SELECT COUNT(*), COALESCE(SUM(amount),0) INTO n_lines, s
    FROM ledger_entries WHERE journal_id = NEW.journal_id;
    IF NEW.id IS NULL THEN n_lines := n_lines + 1; s := s + NEW.amount; END IF;
    IF n_lines >= 2 AND s <> 0 THEN
        RAISE EXCEPTION 'journal_unbalanced';
    END IF;
    SELECT COUNT(DISTINCT COALESCE(la.currency, '')) INTO n_cur
    FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
    WHERE le.journal_id = NEW.journal_id;
    IF n_cur > 1 THEN
        RAISE EXCEPTION 'journal_mixed_currency';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

-- Money rows keep their currency (the ledger accounts their journals
-- posted to are in it); it may only be re-spelled ('usd' -> 'USD',
-- '' -> 'USD'), as the per-currency migration normalizes it.
CREATE OR REPLACE FUNCTION trg_commission_money_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.journal_id IS DISTINCT FROM OLD.journal_id
       OR (CASE WHEN TRIM(NEW.currency) = '' THEN 'USD' ELSE UPPER(TRIM(NEW.currency)) END) IS DISTINCT FROM (CASE WHEN TRIM(OLD.currency) = '' THEN 'USD' ELSE UPPER(TRIM(OLD.currency)) END) THEN
        RAISE EXCEPTION 'commission_immutable';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION trg_payout_money_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.affiliate_id IS DISTINCT FROM OLD.affiliate_id
       OR (CASE WHEN TRIM(NEW.currency) = '' THEN 'USD' ELSE UPPER(TRIM(NEW.currency)) END) IS DISTINCT FROM (CASE WHEN TRIM(OLD.currency) = '' THEN 'USD' ELSE UPPER(TRIM(OLD.currency)) END) THEN
        RAISE EXCEPTION 'payout_immutable';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

-- ---------- journals: append-only (idem_key is the replay guard) ----------

DROP TRIGGER IF EXISTS trg_journal_no_update ON journal_entries;
CREATE TRIGGER trg_journal_no_update BEFORE UPDATE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION forbid('ledger_immutable');

-- ---------- commissions: ledger-backed rows are undeletable ----------

CREATE OR REPLACE FUNCTION trg_commission_no_delete() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger_entries WHERE journal_id = OLD.journal_id) THEN
        RAISE EXCEPTION 'commission_immutable';
    END IF;
    RETURN OLD;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_commission_no_delete ON commissions;
CREATE TRIGGER trg_commission_no_delete BEFORE DELETE ON commissions
    FOR EACH ROW EXECUTE FUNCTION trg_commission_no_delete();

-- ---------- commissions: status machine (see guards_sqlite.sql) ----------
-- Replaces pg.sql's definition: adds paid -> approved for a commission of
-- a reversed payout that is in no payout any more (ReversePaid, MO-16).
CREATE OR REPLACE FUNCTION trg_commission_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status IS DISTINCT FROM NEW.status THEN
        IF NOT (
            (OLD.status = 'pending'  AND NEW.status IN ('approved','cancelled')) OR
            (OLD.status = 'approved' AND NEW.status IN ('paid','cancelled','clawback')) OR
            (OLD.status = 'paid'     AND NEW.status = 'clawback') OR
            (OLD.status = 'paid' AND NEW.status = 'approved'
             AND EXISTS (SELECT 1 FROM payout_reversal_lines rl
                         JOIN payouts p ON p.id = rl.payout_id
                         WHERE rl.commission_id = NEW.id AND p.reversed_at IS NOT NULL)
             AND NOT EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = NEW.id))
        ) THEN
            RAISE EXCEPTION 'illegal_status_transition:%->%', OLD.status, NEW.status;
        END IF;
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

-- ---------- payouts ----------

-- Status machine, exactly the transitions payouts.Runner makes; paid and
-- failed are terminal (see guards_sqlite.sql for the rationale).
CREATE OR REPLACE FUNCTION trg_payout_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status IS DISTINCT FROM NEW.status THEN
        IF NOT (
            (OLD.status = 'requested'  AND NEW.status IN ('approved','processing')) OR
            (OLD.status = 'approved'   AND NEW.status = 'processing') OR
            (OLD.status = 'processing' AND NEW.status IN ('requested','approved','paid','failed')) OR
            (OLD.status = 'paid' AND NEW.status = 'failed' AND NEW.reversed_at IS NOT NULL)
        ) THEN
            RAISE EXCEPTION 'illegal_payout_transition:%->%', OLD.status, NEW.status;
        END IF;
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payout_status ON payouts;
CREATE TRIGGER trg_payout_status BEFORE UPDATE OF status ON payouts
    FOR EACH ROW EXECUTE FUNCTION trg_payout_status();

DROP TRIGGER IF EXISTS trg_payout_no_delete ON payouts;
CREATE TRIGGER trg_payout_no_delete BEFORE DELETE ON payouts
    FOR EACH ROW EXECUTE FUNCTION forbid('payout_immutable');

-- payout_lines: only Runner.Approve releasing a payout it holds in
-- 'processing' (before any rail executes) may delete its lines; see
-- guards_sqlite.sql.
CREATE OR REPLACE FUNCTION trg_payout_lines_no_delete() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM payouts p
                   WHERE p.id = OLD.payout_id
                     AND (p.status = 'processing' OR (p.status = 'failed' AND p.reversed_at IS NOT NULL
                          AND EXISTS (SELECT 1 FROM payout_reversal_lines rl
                                      WHERE rl.payout_id = OLD.payout_id AND rl.commission_id = OLD.commission_id)))) THEN
        RAISE EXCEPTION 'payout_line_immutable';
    END IF;
    RETURN OLD;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payout_lines_no_delete ON payout_lines;
CREATE TRIGGER trg_payout_lines_no_delete BEFORE DELETE ON payout_lines
    FOR EACH ROW EXECUTE FUNCTION trg_payout_lines_no_delete();

-- Export claim is write-once (see guards_sqlite.sql).
CREATE OR REPLACE FUNCTION trg_payout_export_once() RETURNS trigger AS $$
BEGIN
    IF OLD.exported_at IS NOT NULL AND (
        NEW.exported_at IS DISTINCT FROM OLD.exported_at OR
        NEW.export_batch IS DISTINCT FROM OLD.export_batch) THEN
        RAISE EXCEPTION 'payout_export_immutable';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payout_export_once ON payouts;
CREATE TRIGGER trg_payout_export_once BEFORE UPDATE OF exported_at, export_batch ON payouts
    FOR EACH ROW EXECUTE FUNCTION trg_payout_export_once();

-- ---------- TRUNCATE guards (statement-level) ----------

DROP TRIGGER IF EXISTS trg_ledger_entries_no_truncate ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('ledger_immutable');

DROP TRIGGER IF EXISTS trg_journal_no_truncate ON journal_entries;
CREATE TRIGGER trg_journal_no_truncate BEFORE TRUNCATE ON journal_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('ledger_immutable');

DROP TRIGGER IF EXISTS trg_audit_no_truncate ON audit_log;
CREATE TRIGGER trg_audit_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('audit_log is append-only');

DROP TRIGGER IF EXISTS trg_commission_no_truncate ON commissions;
CREATE TRIGGER trg_commission_no_truncate BEFORE TRUNCATE ON commissions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('commission_immutable');

DROP TRIGGER IF EXISTS trg_payout_no_truncate ON payouts;
CREATE TRIGGER trg_payout_no_truncate BEFORE TRUNCATE ON payouts
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('payout_immutable');

DROP TRIGGER IF EXISTS trg_payout_lines_no_truncate ON payout_lines;
CREATE TRIGGER trg_payout_lines_no_truncate BEFORE TRUNCATE ON payout_lines
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('payout_line_immutable');

DROP TRIGGER IF EXISTS trg_raw_events_no_truncate ON raw_events;
CREATE TRIGGER trg_raw_events_no_truncate BEFORE TRUNCATE ON raw_events
    FOR EACH STATEMENT EXECUTE FUNCTION forbid('raw_events_immutable');
