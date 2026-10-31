-- Regular program emails: the owner's Monday report, tips for partners
-- who haven't shared their link a week in, and each partner's monthly
-- recap. One row of switches (absent = all on), and a log so each email
-- goes once per period.
CREATE TABLE IF NOT EXISTS program_emails (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    weekly_report INTEGER NOT NULL DEFAULT 1,
    partner_tips  INTEGER NOT NULL DEFAULT 1,
    partner_recap INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS email_log (
    kind       TEXT NOT NULL,     -- weekly | tips | recap
    subject_id INTEGER NOT NULL,  -- affiliate id; 0 for the owner
    period     TEXT NOT NULL,     -- 2026-W40, 2026-09, once
    sent_at    TEXT NOT NULL,
    PRIMARY KEY (kind, subject_id, period)
);
