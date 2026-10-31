-- Token-dimension analytics (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS token_stats_daily (
    day           INTEGER NOT NULL,
    affiliate_id  BIGINT NOT NULL,
    campaign_id   BIGINT NOT NULL,
    dim           TEXT NOT NULL CHECK (dim IN ('utm_source','subid','placement')),
    value         TEXT NOT NULL,
    clicks        INTEGER NOT NULL DEFAULT 0,
    conversions   INTEGER NOT NULL DEFAULT 0,
    commission_cents BIGINT NOT NULL DEFAULT 0,
    -- Per currency like daily_agg: clicks in the '' row, a conversion's
    -- money in the row of its currency.
    currency      TEXT NOT NULL DEFAULT '',
    CHECK (currency <> '' OR (conversions = 0 AND commission_cents = 0)),
    PRIMARY KEY (day, affiliate_id, campaign_id, dim, value, currency)
);

CREATE TABLE IF NOT EXISTS token_stats_cursor (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    last_click_ts TEXT NOT NULL DEFAULT '',
    last_rollup_day INTEGER NOT NULL DEFAULT 0
);
INSERT INTO token_stats_cursor (id, last_click_ts, last_rollup_day)
VALUES (1, '', 0) ON CONFLICT DO NOTHING;
