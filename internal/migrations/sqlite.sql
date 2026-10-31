-- Ledger core schema — SQLite dialect (PG variant in pg.sql).
-- Ported from the empirically-verified schema (13/13 attack experiments passed;
-- see research/data-layer/ledger.md). All money tables are append-only:
-- triggers reject UPDATE on money fields and DELETE.

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ---------- ingest idempotency (axis 1) ----------

CREATE TABLE IF NOT EXISTS raw_events (
    id                INTEGER PRIMARY KEY,
    source            TEXT NOT NULL,              -- 'stripe', 'paddle', 'api', ...
    external_event_id TEXT NOT NULL,              -- provider event id
    event_type        TEXT NOT NULL,
    payload           TEXT NOT NULL,              -- JSON
    received_at       TEXT NOT NULL DEFAULT (datetime('now')),
    processed_at      TEXT,
    process_error     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_raw_events_source_ext
    ON raw_events(source, external_event_id);
CREATE INDEX IF NOT EXISTS ix_raw_events_unprocessed
    ON raw_events(id) WHERE processed_at IS NULL;

CREATE TABLE IF NOT EXISTS webhook_events (
    id                INTEGER PRIMARY KEY,
    source            TEXT NOT NULL,
    external_event_id TEXT NOT NULL,
    signature_ok      INTEGER NOT NULL DEFAULT 0,
    payload           TEXT NOT NULL,
    received_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_webhook_events_source_ext
    ON webhook_events(source, external_event_id);

-- ---------- affiliates / programs (minimal for M0) ----------

CREATE TABLE IF NOT EXISTS affiliates (
    id           INTEGER PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','frozen','removed')),
    postback_url TEXT NOT NULL DEFAULT '',   -- S2S conversion postback template
    sponsor_id          INTEGER,             -- SPONSOR tree: unilevel upline
    placement_parent_id INTEGER,             -- PLACEMENT tree: binary pairing
    placement_leg       TEXT CHECK (placement_leg IN ('L','R')),
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ---------- ledger accounts ----------

CREATE TABLE IF NOT EXISTS ledger_accounts (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,             -- 'AFFILIATE_PAYABLE:<id>', 'HOLD', ...
    kind        TEXT NOT NULL CHECK (kind IN ('asset','liability','expense','clearing')),
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ---------- double-entry journal (the money authority) ----------

CREATE TABLE IF NOT EXISTS journal_entries (
    id           INTEGER PRIMARY KEY,             -- gapless: AUTOINCREMENT-style via trigger below
    idem_key     TEXT NOT NULL UNIQUE,            -- replay guard (axis 2 per-journal)
    source       TEXT NOT NULL,                   -- 'conversion','maturation','payout','clawback','adjustment'
    description  TEXT NOT NULL DEFAULT '',
    effective_at TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id          INTEGER PRIMARY KEY,
    journal_id  INTEGER NOT NULL REFERENCES journal_entries(id),
    account_id  INTEGER NOT NULL REFERENCES ledger_accounts(id),
    amount      INTEGER NOT NULL,                 -- minor units, signed; journal must sum to 0
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS ix_ledger_entries_account ON ledger_entries(account_id, id);
CREATE INDEX IF NOT EXISTS ix_ledger_entries_journal ON ledger_entries(journal_id);

-- Posting procedure: inserts a journal + lines atomically and validates
-- sum=0, >=2 lines, and no AFFILIATE_PAYABLE overdraft. Implemented in Go
-- (internal/ledger.Post) inside one transaction; the trigger below is the
-- last-line-of-defense validator.
CREATE TRIGGER IF NOT EXISTS trg_journal_balanced
AFTER INSERT ON ledger_entries
BEGIN
    SELECT CASE
        WHEN (SELECT COUNT(*) FROM ledger_entries WHERE journal_id = NEW.journal_id) >= 2
         AND (SELECT COALESCE(SUM(amount),0) FROM ledger_entries WHERE journal_id = NEW.journal_id) != 0
        THEN RAISE(ABORT, 'journal_unbalanced')
    END;
END;

-- Immutability: money rows are append-only.
CREATE TRIGGER IF NOT EXISTS trg_ledger_entries_no_update BEFORE UPDATE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_ledger_entries_no_delete BEFORE DELETE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_journal_no_delete BEFORE DELETE ON journal_entries
BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END;

-- ---------- commissions (domain facts) ----------

CREATE TABLE IF NOT EXISTS commissions (
    id                   INTEGER PRIMARY KEY,
    affiliate_id         INTEGER NOT NULL REFERENCES affiliates(id),
    campaign_id          INTEGER NOT NULL DEFAULT 1,
    journal_id           INTEGER NOT NULL REFERENCES journal_entries(id),
    amount               INTEGER NOT NULL CHECK (amount != 0),  -- minor units; negative = clawback
    currency             TEXT NOT NULL DEFAULT 'USD',
    rate_snapshot        TEXT NOT NULL,                          -- JSON of the rule at credit time
    status               TEXT NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','approved','paid',
                                               'cancelled','clawback')),
    matures_at           TEXT,
    approved_at          TEXT,
    paid_at              TEXT,
    parent_commission_id INTEGER REFERENCES commissions(id),     -- clawback linkage
    split_group_id       INTEGER,                                -- N-rows-per-conversion grouping
    created_at           TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS ix_commissions_affiliate_status
    ON commissions(affiliate_id, status);
CREATE INDEX IF NOT EXISTS ix_commissions_parent ON commissions(parent_commission_id)
    WHERE parent_commission_id IS NOT NULL;

-- State machine: only legal transitions.
CREATE TRIGGER IF NOT EXISTS trg_commission_status
BEFORE UPDATE OF status ON commissions
WHEN OLD.status != NEW.status
BEGIN
    SELECT CASE
        WHEN (OLD.status = 'pending'  AND NEW.status IN ('approved','cancelled'))
          OR (OLD.status = 'approved' AND NEW.status IN ('paid','cancelled','clawback'))
          OR (OLD.status = 'paid'     AND NEW.status = 'clawback')
        THEN 1
        ELSE RAISE(ABORT, 'illegal_status_transition:' || OLD.status || '->' || NEW.status)
    END;
END;

-- Money-field immutability after insert.
CREATE TRIGGER IF NOT EXISTS trg_commission_money_immutable
BEFORE UPDATE ON commissions
WHEN OLD.status != 'pending' OR NEW.amount != OLD.amount OR NEW.journal_id != OLD.journal_id
BEGIN
    SELECT CASE
        WHEN NEW.amount != OLD.amount OR NEW.journal_id != OLD.journal_id
        THEN RAISE(ABORT, 'commission_immutable')
    END;
END;

-- Timestamp-format heal (one-time, self-guarding): rows written by older
-- builds bound Go time.Time directly, which the SQLite driver renders as
-- t.String() ("2026-09-17 03:12:45 +0000 UTC"). All writers/compares now
-- use RFC3339(Nano); un-healed legacy rows would misorder against them
-- within the same day (space < 'T'), maturing commissions up to 24h early
-- and mis-bucketing MLM/fraud windows. Only touches columns the immutability
-- trigger permits (amount/journal_id are untouched).
UPDATE commissions SET
    created_at = replace(replace(created_at, ' +0000 UTC', 'Z'), ' ', 'T'),
    matures_at = replace(replace(matures_at, ' +0000 UTC', 'Z'), ' ', 'T'),
    approved_at = replace(replace(approved_at, ' +0000 UTC', 'Z'), ' ', 'T'),
    paid_at     = replace(replace(paid_at,     ' +0000 UTC', 'Z'), ' ', 'T')
WHERE created_at LIKE '% +0000 UTC'
   OR matures_at LIKE '% +0000 UTC'
   OR approved_at LIKE '% +0000 UTC'
   OR paid_at LIKE '% +0000 UTC';
UPDATE affiliates SET
    created_at = replace(replace(created_at, ' +0000 UTC', 'Z'), ' ', 'T')
WHERE created_at LIKE '% +0000 UTC';

-- ---------- payouts ----------

CREATE TABLE IF NOT EXISTS payouts (
    id           INTEGER PRIMARY KEY,
    affiliate_id INTEGER NOT NULL REFERENCES affiliates(id),
    amount       INTEGER NOT NULL CHECK (amount > 0),
    currency     TEXT NOT NULL DEFAULT 'USD',
    method       TEXT NOT NULL DEFAULT 'manual',
    status       TEXT NOT NULL DEFAULT 'requested'
                     CHECK (status IN ('requested','approved','processing','paid','failed')),
    journal_id   INTEGER REFERENCES journal_entries(id),
    provider_ref TEXT,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    processed_at TEXT
);

CREATE TRIGGER IF NOT EXISTS trg_payout_money_immutable BEFORE UPDATE ON payouts
WHEN NEW.amount != OLD.amount OR NEW.affiliate_id != OLD.affiliate_id
BEGIN SELECT RAISE(ABORT, 'payout_immutable'); END;

CREATE TABLE IF NOT EXISTS payout_lines (
    payout_id     INTEGER NOT NULL REFERENCES payouts(id),
    commission_id INTEGER NOT NULL REFERENCES commissions(id),
    PRIMARY KEY (payout_id, commission_id)
);

-- A commission lives in at most ONE payout (whole-commission coverage,
-- INV-10). Concurrent CreateBatch runs must not double-stage: the NOT
-- EXISTS guard in CreateBatch is snapshot-blind between transactions, so
-- the database enforces it (attack 10 pin).
CREATE UNIQUE INDEX IF NOT EXISTS uq_payout_line_commission
    ON payout_lines (commission_id);

-- ---------- outbox (transactional, same-txn as money writes) ----------

CREATE TABLE IF NOT EXISTS outbox_events (
    id          INTEGER PRIMARY KEY,
    topic       TEXT NOT NULL,                    -- 'commission.created', 'payout.paid', ...
    payload     TEXT NOT NULL,                    -- JSON
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    delivered_at TEXT,
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT
);
CREATE INDEX IF NOT EXISTS ix_outbox_undelivered ON outbox_events(id)
    WHERE delivered_at IS NULL;

-- Buyer email for fraud correlation (payment fingerprint) + operator
-- visibility. Idempotent.
ALTER TABLE commissions ADD COLUMN buyer_email TEXT NOT NULL DEFAULT '';
