-- Gamification layer v1 (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS xp_events (
    id            BIGSERIAL PRIMARY KEY,
    affiliate_id  BIGINT NOT NULL REFERENCES affiliates(id),
    reason        TEXT NOT NULL,
    xp            INTEGER NOT NULL CHECK (xp > 0),
    source_ref    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (affiliate_id, reason, source_ref)
);

CREATE TABLE IF NOT EXISTS xp_totals (
    affiliate_id  BIGINT PRIMARY KEY REFERENCES affiliates(id),
    xp            INTEGER NOT NULL DEFAULT 0,
    level         INTEGER NOT NULL DEFAULT 1,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS badges (
    id            BIGSERIAL PRIMARY KEY,
    code          TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    icon          TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS affiliate_badges (
    affiliate_id  BIGINT NOT NULL REFERENCES affiliates(id),
    badge_code    TEXT NOT NULL REFERENCES badges(code),
    awarded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (affiliate_id, badge_code)
);
