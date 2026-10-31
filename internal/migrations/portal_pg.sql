-- Affiliate portal auth (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS affiliate_sessions (
    token_hash   TEXT PRIMARY KEY,
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_aff_sessions_exp ON affiliate_sessions(expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS affiliate_applications (
    id          BIGSERIAL PRIMARY KEY,
    email       TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','approved','rejected')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS portal_login_grants (
    token_hash   TEXT PRIMARY KEY,
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ
);
