-- Partner resources: what the owner gives partners to promote with
-- (ready-to-copy text, banners and logos, useful links). Shown in the
-- partner portal. Images are public (partners put them on their own
-- sites); text and links are for signed-in partners.
CREATE TABLE IF NOT EXISTS partner_resources (
    id           INTEGER PRIMARY KEY,
    campaign_id  INTEGER NOT NULL DEFAULT 1,
    kind         TEXT NOT NULL CHECK (kind IN ('text','image','link')),
    title        TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',   -- text: the copy; link: the URL
    content_type TEXT NOT NULL DEFAULT '',   -- image only
    data         BLOB,                       -- image only
    sha256       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS ix_partner_resources_campaign ON partner_resources(campaign_id, id);
