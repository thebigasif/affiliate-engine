-- Referral codes (PostgreSQL dialect; mirror of codes_sqlite.sql).

CREATE TABLE IF NOT EXISTS affiliate_codes (
    id           BIGSERIAL PRIMARY KEY,
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    code         TEXT NOT NULL UNIQUE CHECK (code = lower(code)),
    is_primary   BOOLEAN NOT NULL DEFAULT false,
    source       TEXT NOT NULL DEFAULT 'native',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    campaign_id  BIGINT,
    UNIQUE (affiliate_id, code)
);
CREATE INDEX IF NOT EXISTS ix_aff_codes_aff ON affiliate_codes(affiliate_id, is_primary);
