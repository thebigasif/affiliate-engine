-- Uploaded program logos (FR-33). PG mirror of brandlogo_sqlite.sql.
CREATE TABLE IF NOT EXISTS program_logos (
    campaign_id  BIGINT PRIMARY KEY REFERENCES campaigns(id),
    content_type TEXT NOT NULL,
    data         BYTEA NOT NULL,
    sha256       TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
