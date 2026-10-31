-- Tax-form uploads (FR-17 portal flow). An affiliate uploads a W-9 /
-- W-8BEN / W-8BEN-E PDF from the portal; the bytes live here, sealed with
-- AES-256-GCM under a key derived from the deployment secret, so a leaked
-- DB file or backup alone does not expose SSNs/EINs. One current file per
-- tax_documents row: a re-upload replaces it. Never served by the static
-- file server; only the admin API decrypts and returns it, as an
-- attachment. GDPR erase deletes these rows with the tax_documents row.
CREATE TABLE IF NOT EXISTS tax_document_files (
    id               INTEGER PRIMARY KEY,
    tax_document_id  INTEGER NOT NULL UNIQUE REFERENCES tax_documents(id),
    affiliate_id     INTEGER NOT NULL REFERENCES affiliates(id),
    file_name        TEXT NOT NULL DEFAULT '',
    size_bytes       INTEGER NOT NULL,
    sha256           TEXT NOT NULL,        -- hex digest of the plaintext
    sealed           BLOB NOT NULL,        -- nonce || AES-GCM ciphertext
    uploaded_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS ix_taxdocfiles_aff ON tax_document_files(affiliate_id);
