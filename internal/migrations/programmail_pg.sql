-- Regular program emails. PG mirror of programmail_sqlite.sql.
CREATE TABLE IF NOT EXISTS program_emails (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    weekly_report INTEGER NOT NULL DEFAULT 1,
    partner_tips  INTEGER NOT NULL DEFAULT 1,
    partner_recap INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS email_log (
    kind       TEXT NOT NULL,
    subject_id BIGINT NOT NULL,
    period     TEXT NOT NULL,
    sent_at    TEXT NOT NULL,
    PRIMARY KEY (kind, subject_id, period)
);
