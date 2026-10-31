-- Payout-details reminders. PG mirror of payoutask_sqlite.sql.
CREATE TABLE IF NOT EXISTS payout_details_asks (
    affiliate_id BIGINT PRIMARY KEY REFERENCES affiliates(id),
    asked_at     TEXT NOT NULL
);
