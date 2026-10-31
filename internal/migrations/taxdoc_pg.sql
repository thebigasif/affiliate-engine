-- Tax-document gating (FR-17): PG mirror. Same gate semantics as the
-- SQLite dialect; trigger body in plpgsql.

CREATE TABLE IF NOT EXISTS tax_documents (
    id            BIGSERIAL PRIMARY KEY,
    affiliate_id  BIGINT NOT NULL REFERENCES affiliates(id),
    doc_type      TEXT NOT NULL CHECK (doc_type IN ('w9','w8ben','w8bene','other')),
    status        TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','received','validated','rejected','not_required')),
    reference     TEXT NOT NULL DEFAULT '',
    note          TEXT NOT NULL DEFAULT '',
    reviewed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (affiliate_id, doc_type)
);
CREATE INDEX IF NOT EXISTS ix_taxdocs_aff ON tax_documents(affiliate_id);

CREATE OR REPLACE FUNCTION audit_taxdoc_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status IS DISTINCT FROM NEW.status THEN
        INSERT INTO audit_log (table_name, row_id, action, detail)
        VALUES ('tax_documents', NEW.id, 'status', OLD.status || '->' || NEW.status);
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_taxdoc_status ON tax_documents;
CREATE TRIGGER trg_audit_taxdoc_status AFTER UPDATE OF status ON tax_documents
    FOR EACH ROW EXECUTE FUNCTION audit_taxdoc_status();
