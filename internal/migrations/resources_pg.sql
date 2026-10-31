-- Partner resources. PG mirror of resources_sqlite.sql.
CREATE TABLE IF NOT EXISTS partner_resources (
    id           BIGSERIAL PRIMARY KEY,
    campaign_id  BIGINT NOT NULL DEFAULT 1,
    kind         TEXT NOT NULL CHECK (kind IN ('text','image','link')),
    title        TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT '',
    data         BYTEA,
    sha256       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_partner_resources_campaign ON partner_resources(campaign_id, id);
