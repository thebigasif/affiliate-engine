-- Datacenter-click tracking (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS dc_click_daily (
    day           INTEGER NOT NULL,
    affiliate_id  BIGINT NOT NULL,
    dc_clicks     INTEGER NOT NULL DEFAULT 0,
    total_clicks  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, affiliate_id)
);
