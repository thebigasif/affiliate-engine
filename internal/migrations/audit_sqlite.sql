-- Audit log (NFR-9): append-only record of money-state mutations. The
-- enforcement is the same trigger discipline as the ledger: INSERT-only
-- table, no UPDATE/DELETE paths, gapless sequence. (SQLite dialect; PG
-- mirror in audit_pg.sql.)

CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (datetime('now')),
    actor       TEXT NOT NULL DEFAULT 'system',  -- 'system' | 'admin:<id>' | 'affiliate:<id>' | 'webhook:<source>'
    table_name  TEXT NOT NULL,
    row_id      INTEGER NOT NULL,
    action      TEXT NOT NULL,                    -- 'insert' | 'status'
    detail      TEXT NOT NULL DEFAULT ''          -- e.g. 'approved->paid'
);

-- Append-only enforcement (same shape as the ledger's journal guards).
CREATE TRIGGER IF NOT EXISTS trg_audit_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_audit_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;

-- Commission status transitions are THE money-relevant mutation class:
-- every change lands in the audit log with old->new.
CREATE TRIGGER IF NOT EXISTS trg_audit_commission_status
AFTER UPDATE OF status ON commissions
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('commissions', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;

-- Payout status transitions likewise.
CREATE TRIGGER IF NOT EXISTS trg_audit_payout_status
AFTER UPDATE OF status ON payouts
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('payouts', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;

-- Affiliate status (freezes are fraud enforcement).
CREATE TRIGGER IF NOT EXISTS trg_audit_affiliate_status
AFTER UPDATE OF status ON affiliates
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('affiliates', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;
