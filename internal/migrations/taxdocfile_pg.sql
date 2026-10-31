-- Tax-form uploads (FR-17 portal flow). PG mirror of taxdocfile_sqlite.sql.
CREATE TABLE IF NOT EXISTS tax_document_files (
    id               BIGSERIAL PRIMARY KEY,
    tax_document_id  BIGINT NOT NULL UNIQUE REFERENCES tax_documents(id),
    affiliate_id     BIGINT NOT NULL REFERENCES affiliates(id),
    file_name        TEXT NOT NULL DEFAULT '',
    size_bytes       BIGINT NOT NULL,
    sha256           TEXT NOT NULL,
    sealed           BYTEA NOT NULL,
    uploaded_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_taxdocfiles_aff ON tax_document_files(affiliate_id);
