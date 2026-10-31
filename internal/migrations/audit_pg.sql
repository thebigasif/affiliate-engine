-- Audit log (NFR-9): PG mirror. Trigger bodies in plpgsql.

CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGSERIAL PRIMARY KEY,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor       TEXT NOT NULL DEFAULT 'system',
    table_name  TEXT NOT NULL,
    row_id      BIGINT NOT NULL,
    action      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT ''
);

CREATE OR REPLACE FUNCTION audit_log_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_no_update ON audit_log;
CREATE TRIGGER trg_audit_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

CREATE OR REPLACE FUNCTION audit_commission_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status != NEW.status THEN
        INSERT INTO audit_log (table_name, row_id, action, detail)
        VALUES ('commissions', NEW.id, 'status', OLD.status || '->' || NEW.status);
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_commission_status ON commissions;
CREATE TRIGGER trg_audit_commission_status AFTER UPDATE OF status ON commissions
    FOR EACH ROW EXECUTE FUNCTION audit_commission_status();

CREATE OR REPLACE FUNCTION audit_payout_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status != NEW.status THEN
        INSERT INTO audit_log (table_name, row_id, action, detail)
        VALUES ('payouts', NEW.id, 'status', OLD.status || '->' || NEW.status);
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_payout_status ON payouts;
CREATE TRIGGER trg_audit_payout_status AFTER UPDATE OF status ON payouts
    FOR EACH ROW EXECUTE FUNCTION audit_payout_status();

CREATE OR REPLACE FUNCTION audit_affiliate_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status != NEW.status THEN
        INSERT INTO audit_log (table_name, row_id, action, detail)
        VALUES ('affiliates', NEW.id, 'status', OLD.status || '->' || NEW.status);
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_affiliate_status ON affiliates;
CREATE TRIGGER trg_audit_affiliate_status AFTER UPDATE OF status ON affiliates
    FOR EACH ROW EXECUTE FUNCTION audit_affiliate_status();
