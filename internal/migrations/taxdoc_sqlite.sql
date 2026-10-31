-- Tax-document gating (FR-17): payout-side compliance gate. The operator
-- records each affiliate's tax-doc STATUS here. The document itself (W-9 /
-- W-8BEN PDF with SSN/EIN) is either collected out-of-band or uploaded from
-- the portal into tax_document_files (sealed; see taxdocfile_*.sql). Rows with
-- status 'pending' or 'rejected' BLOCK payout batching for that affiliate
-- (see payouts.Runner.CreateBatch); 'received'/'validated'/'not_required'
-- unblock. Absence of a row = no requirement recorded = not blocking
-- (backwards compatible with every existing install).

CREATE TABLE IF NOT EXISTS tax_documents (
    id            INTEGER PRIMARY KEY,
    affiliate_id  INTEGER NOT NULL REFERENCES affiliates(id),
    doc_type      TEXT NOT NULL CHECK (doc_type IN ('w9','w8ben','w8bene','other')),
    status        TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','received','validated','rejected','not_required')),
    reference     TEXT NOT NULL DEFAULT '',  -- operator's external reference (collector id, form #)
    note          TEXT NOT NULL DEFAULT '',
    reviewed_at   TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (affiliate_id, doc_type)
);
CREATE INDEX IF NOT EXISTS ix_taxdocs_aff ON tax_documents(affiliate_id);

-- Every gate decision is auditable (NFR-9): status transitions land in the
-- append-only audit log with old->new.
CREATE TRIGGER IF NOT EXISTS trg_audit_taxdoc_status
AFTER UPDATE OF status ON tax_documents
WHEN OLD.status != NEW.status
BEGIN
    INSERT INTO audit_log (table_name, row_id, action, detail)
    VALUES ('tax_documents', NEW.id, 'status', OLD.status || '->' || NEW.status);
END;
