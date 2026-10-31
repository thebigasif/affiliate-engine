-- When each partner was last emailed to add where to be paid (a payout
-- was waiting and they hadn't said). Re-asked at most once a week.
CREATE TABLE IF NOT EXISTS payout_details_asks (
    affiliate_id INTEGER PRIMARY KEY REFERENCES affiliates(id),
    asked_at     TEXT NOT NULL
);
