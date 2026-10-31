-- The referral code a click came through (/r/{code}), lower-cased; '' for
-- the numeric-id fallback and for clicks recorded before this column. Lets
-- import undo see whether a code the batch created has been used since
-- (on any affiliate, including pre-existing ones), and refuse.
ALTER TABLE clicks ADD COLUMN ref_code TEXT NOT NULL DEFAULT '';
