-- Gamification layer v1 (SQLite dialect): XP events, badges, leaderboard.
-- XP derives ONLY from money/ledger facts (never free-floating counters —
-- the same rule as every other number in the system).

CREATE TABLE IF NOT EXISTS xp_events (
    id            INTEGER PRIMARY KEY,
    affiliate_id  INTEGER NOT NULL REFERENCES affiliates(id),
    reason        TEXT NOT NULL,             -- 'first_commission','conversion','milestone_1k',...
    xp            INTEGER NOT NULL CHECK (xp > 0),
    source_ref    TEXT NOT NULL DEFAULT '',  -- commission id / click day etc.
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (affiliate_id, reason, source_ref)  -- exactly-once XP grants
);

CREATE TABLE IF NOT EXISTS xp_totals (
    affiliate_id  INTEGER PRIMARY KEY REFERENCES affiliates(id),
    xp            INTEGER NOT NULL DEFAULT 0,
    level         INTEGER NOT NULL DEFAULT 1,
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS badges (
    id            INTEGER PRIMARY KEY,
    code          TEXT NOT NULL UNIQUE,       -- 'first_commission', 'earner_1k', ...
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    icon          TEXT NOT NULL DEFAULT ''    -- emoji/short glyph for the portal
);

CREATE TABLE IF NOT EXISTS affiliate_badges (
    affiliate_id  INTEGER NOT NULL REFERENCES affiliates(id),
    badge_code    TEXT NOT NULL REFERENCES badges(code),
    awarded_at    TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (affiliate_id, badge_code)
);
