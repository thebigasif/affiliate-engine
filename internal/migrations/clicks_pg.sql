-- Wave 1 additions: click pipeline + attribution (PostgreSQL dialect).
-- Mirrors clicks_sqlite.sql.

CREATE TABLE IF NOT EXISTS clicks (
    id           TEXT PRIMARY KEY,
    ts           TIMESTAMPTZ NOT NULL,
    day          INTEGER NOT NULL,
    month        INTEGER NOT NULL,
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    campaign_id  BIGINT NOT NULL DEFAULT 1,
    ip_hash      BYTEA,
    ua_hash      BYTEA,
    referer      TEXT NOT NULL DEFAULT '',
    landing_path TEXT NOT NULL DEFAULT '',
    utm_source   TEXT NOT NULL DEFAULT '',
    subid        TEXT NOT NULL DEFAULT '',
    placement    TEXT NOT NULL DEFAULT '',
    extra_json   JSONB NOT NULL DEFAULT '{}',
    bot_class    SMALLINT NOT NULL DEFAULT 0,
    exclude_stats SMALLINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_clicks_day ON clicks(day);
CREATE INDEX IF NOT EXISTS ix_clicks_aff_day ON clicks(affiliate_id, day);
CREATE INDEX IF NOT EXISTS ix_clicks_ip_day ON clicks(ip_hash, day) WHERE ip_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_clicks_place ON clicks(placement, affiliate_id, day);

CREATE TABLE IF NOT EXISTS daily_agg (
    day          INTEGER NOT NULL,
    affiliate_id BIGINT NOT NULL,
    campaign_id  BIGINT NOT NULL,
    clicks       BIGINT NOT NULL DEFAULT 0,
    conversions  BIGINT NOT NULL DEFAULT 0,
    commission_cents BIGINT NOT NULL DEFAULT 0,
    -- Money is per currency: a sale's conversions and commission are in
    -- the row of its currency; clicks have none and live in the '' row.
    -- Databases from before the currency column are rebuilt by
    -- aggregatesPerCurrency (aggcurrency.go).
    currency     TEXT NOT NULL DEFAULT '',
    CHECK (currency <> '' OR (conversions = 0 AND commission_cents = 0)),
    PRIMARY KEY (day, affiliate_id, campaign_id, currency)
);

CREATE TABLE IF NOT EXISTS uniq_daily (
    day          INTEGER NOT NULL,
    affiliate_id BIGINT NOT NULL,
    ip_hash      BYTEA NOT NULL,
    PRIMARY KEY (day, affiliate_id, ip_hash)
);

CREATE TABLE IF NOT EXISTS watermark (
    name     TEXT PRIMARY KEY,
    last_id  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS attributions (
    id           BIGSERIAL PRIMARY KEY,
    click_id     TEXT NOT NULL REFERENCES clicks(id),
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    campaign_id  BIGINT NOT NULL DEFAULT 1,
    method       TEXT NOT NULL CHECK (method IN ('coupon','token','customer','ip_fallback')),
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_attributions_click ON attributions(click_id);
CREATE INDEX IF NOT EXISTS ix_attributions_aff ON attributions(affiliate_id, created_at);

CREATE TABLE IF NOT EXISTS customers (
    id                   BIGSERIAL PRIMARY KEY,
    program_id           BIGINT NOT NULL DEFAULT 1,
    external_customer_id TEXT NOT NULL,
    affiliate_id         BIGINT NOT NULL REFERENCES affiliates(id),
    bound_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    bound_method         TEXT NOT NULL DEFAULT 'token',
    UNIQUE (program_id, external_customer_id)
);

CREATE TABLE IF NOT EXISTS coupons (
    id           BIGSERIAL PRIMARY KEY,
    code         TEXT NOT NULL UNIQUE,
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    campaign_id  BIGINT NOT NULL DEFAULT 1,
    rate_type    TEXT NOT NULL DEFAULT 'inherit' CHECK (rate_type IN ('inherit','percent','fixed')),
    rate_value   BIGINT NOT NULL DEFAULT 0,
    active       BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS commission_rules (
    id           BIGSERIAL PRIMARY KEY,
    campaign_id  BIGINT NOT NULL DEFAULT 1,
    affiliate_id BIGINT,
    event_type   TEXT NOT NULL DEFAULT 'purchase',
    rate_type    TEXT NOT NULL CHECK (rate_type IN ('percent','fixed')),
    rate_value   BIGINT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    active       BOOLEAN NOT NULL DEFAULT true
);
