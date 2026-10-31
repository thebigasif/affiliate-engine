-- Datacenter-click tracking: ingest-time classification (privacy-first:
-- no raw IPs stored; the flag is derived at the edge and persisted).
-- SQLite dialect.

CREATE TABLE IF NOT EXISTS dc_click_daily (
    day           INTEGER NOT NULL,
    affiliate_id  INTEGER NOT NULL,
    dc_clicks     INTEGER NOT NULL DEFAULT 0,
    total_clicks  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, affiliate_id)
);
