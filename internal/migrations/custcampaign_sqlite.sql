-- The program (campaign) a customer was bound under. Renewals of a bound
-- customer credit in that program, at its rates, hold days and commission
-- months. Existing bindings were all made under program 1.
ALTER TABLE customers ADD COLUMN campaign_id INTEGER NOT NULL DEFAULT 1;
