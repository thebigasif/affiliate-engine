-- Referral codes (SQLite dialect): human slugs for /r/{code}, the substrate
-- for importer slug preservation (FR-32: an affiliate's incumbent links keep
-- working after migration). Codes are stored lowercase; UNIQUE(code) is the
-- case-insensitive guarantee because writers normalize.

CREATE TABLE IF NOT EXISTS affiliate_codes (
    id           INTEGER PRIMARY KEY,
    affiliate_id INTEGER NOT NULL REFERENCES affiliates(id),
    code         TEXT NOT NULL UNIQUE CHECK (code = lower(code)),
    is_primary   INTEGER NOT NULL DEFAULT 0,   -- shown in the portal link generator
    source       TEXT NOT NULL DEFAULT 'native', -- 'native' | 'import:rewardful' | ...
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    campaign_id  INTEGER,                          -- per-program links (NULL = default program)
    UNIQUE (affiliate_id, code)
);
CREATE INDEX IF NOT EXISTS ix_aff_codes_aff ON affiliate_codes(affiliate_id, is_primary);
