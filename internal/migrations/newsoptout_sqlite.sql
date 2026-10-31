-- Partners who unsubscribed from program news emails (sign-in links and
-- payout notes still go out: they're about their account).
ALTER TABLE affiliates ADD COLUMN news_opt_out INTEGER NOT NULL DEFAULT 0;
