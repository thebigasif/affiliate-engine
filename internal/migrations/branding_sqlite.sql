-- White-label branding (SQLite dialect, FR-33): per-program portal identity
-- + custom domain + widget flag. Empty values = engine defaults (core tier
-- degrades gracefully; branding is cosmetic, never money-affecting).

CREATE TABLE IF NOT EXISTS program_branding (
    campaign_id    INTEGER PRIMARY KEY REFERENCES campaigns(id),
    brand_name     TEXT NOT NULL DEFAULT '',
    accent_color   TEXT NOT NULL DEFAULT '',   -- '#RRGGBB' or '' (default blue)
    logo_url       TEXT NOT NULL DEFAULT '',   -- https only, enforced at write
    portal_domain  TEXT NOT NULL DEFAULT '',   -- lowercase host, no scheme/path
    widget_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Custom-domain resolution: one program per domain (partial index keeps
-- unbranded rows out of the uniqueness domain).
CREATE UNIQUE INDEX IF NOT EXISTS ix_branding_domain
    ON program_branding(portal_domain) WHERE portal_domain <> '';
