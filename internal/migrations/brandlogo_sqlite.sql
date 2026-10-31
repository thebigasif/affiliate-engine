-- Uploaded program logos (FR-33). Small images (PNG, JPEG, WebP or GIF,
-- up to 256 KB) the admin uploads from Settings -> Brand. Served same-origin
-- by /api/v1/portal/logo, so the dashboard CSP needs no outside image hosts.
CREATE TABLE IF NOT EXISTS program_logos (
    campaign_id  INTEGER PRIMARY KEY REFERENCES campaigns(id),
    content_type TEXT NOT NULL,
    data         BLOB NOT NULL,
    sha256       TEXT NOT NULL,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
