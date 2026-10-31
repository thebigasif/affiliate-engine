-- Ledger core schema — PostgreSQL dialect (SQLite variant in sqlite.sql).
-- Same invariants, PG-native enforcement. Ported from the empirically-verified
-- schema (research/data-layer/ledger.md).

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS raw_events (
    id                BIGSERIAL PRIMARY KEY,
    source            TEXT NOT NULL,
    external_event_id TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    payload           JSONB NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    process_error     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_raw_events_source_ext
    ON raw_events(source, external_event_id);
CREATE INDEX IF NOT EXISTS ix_raw_events_unprocessed
    ON raw_events(id) WHERE processed_at IS NULL;

CREATE TABLE IF NOT EXISTS webhook_events (
    id                BIGSERIAL PRIMARY KEY,
    source            TEXT NOT NULL,
    external_event_id TEXT NOT NULL,
    signature_ok      BOOLEAN NOT NULL DEFAULT false,
    payload           JSONB NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_webhook_events_source_ext
    ON webhook_events(source, external_event_id);

CREATE TABLE IF NOT EXISTS affiliates (
    id           BIGSERIAL PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL DEFAULT '',
    postback_url TEXT NOT NULL DEFAULT '',
    sponsor_id          BIGINT,
    placement_parent_id BIGINT,
    placement_leg       TEXT CHECK (placement_leg IN ('L','R')),
    status       TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','frozen','removed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ledger_accounts (
    id         BIGSERIAL PRIMARY KEY,
    code       TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL CHECK (kind IN ('asset','liability','expense','clearing')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS journal_entries (
    id           BIGSERIAL PRIMARY KEY,
    idem_key     TEXT NOT NULL UNIQUE,
    source       TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    effective_at TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id         BIGSERIAL PRIMARY KEY,
    journal_id BIGINT NOT NULL REFERENCES journal_entries(id),
    account_id BIGINT NOT NULL REFERENCES ledger_accounts(id),
    amount     BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_ledger_entries_account ON ledger_entries(account_id, id);
CREATE INDEX IF NOT EXISTS ix_ledger_entries_journal ON ledger_entries(journal_id);

CREATE OR REPLACE FUNCTION trg_journal_balanced() RETURNS trigger AS $$
DECLARE
    n_lines BIGINT; s BIGINT;
BEGIN
    SELECT COUNT(*), COALESCE(SUM(amount),0) INTO n_lines, s
    FROM ledger_entries WHERE journal_id = NEW.journal_id;
    IF NEW.id IS NULL THEN n_lines := n_lines + 1; s := s + NEW.amount; END IF;
    IF n_lines >= 2 AND s <> 0 THEN
        RAISE EXCEPTION 'journal_unbalanced';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_journal_balanced ON ledger_entries;
CREATE TRIGGER trg_journal_balanced
AFTER INSERT ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION trg_journal_balanced();

CREATE OR REPLACE FUNCTION forbid() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION '%', TG_ARGV[0]; END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ledger_entries_no_update ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_no_update BEFORE UPDATE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION forbid('ledger_immutable');
DROP TRIGGER IF EXISTS trg_ledger_entries_no_delete ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_no_delete BEFORE DELETE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION forbid('ledger_immutable');
DROP TRIGGER IF EXISTS trg_journal_no_delete ON journal_entries;
CREATE TRIGGER trg_journal_no_delete BEFORE DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION forbid('ledger_immutable');

CREATE TABLE IF NOT EXISTS commissions (
    id                   BIGSERIAL PRIMARY KEY,
    affiliate_id         BIGINT NOT NULL REFERENCES affiliates(id),
    campaign_id          BIGINT NOT NULL DEFAULT 1,
    journal_id           BIGINT NOT NULL REFERENCES journal_entries(id),
    amount               BIGINT NOT NULL CHECK (amount != 0), -- negative = clawback
    currency             TEXT NOT NULL DEFAULT 'USD',
    rate_snapshot        JSONB NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','approved','paid',
                                               'cancelled','clawback')),
    matures_at           TIMESTAMPTZ,
    approved_at          TIMESTAMPTZ,
    paid_at              TIMESTAMPTZ,
    parent_commission_id BIGINT REFERENCES commissions(id),
    split_group_id       BIGINT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ix_commissions_affiliate_status
    ON commissions(affiliate_id, status);
CREATE INDEX IF NOT EXISTS ix_commissions_parent ON commissions(parent_commission_id)
    WHERE parent_commission_id IS NOT NULL;

CREATE OR REPLACE FUNCTION trg_commission_status() RETURNS trigger AS $$
BEGIN
    IF OLD.status IS DISTINCT FROM NEW.status THEN
        IF NOT (
            (OLD.status = 'pending'  AND NEW.status IN ('approved','cancelled')) OR
            (OLD.status = 'approved' AND NEW.status IN ('paid','cancelled','clawback')) OR
            (OLD.status = 'paid'     AND NEW.status = 'clawback')
        ) THEN
            RAISE EXCEPTION 'illegal_status_transition:%->%', OLD.status, NEW.status;
        END IF;
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_commission_status ON commissions;
CREATE TRIGGER trg_commission_status BEFORE UPDATE OF status ON commissions
FOR EACH ROW EXECUTE FUNCTION trg_commission_status();

CREATE OR REPLACE FUNCTION trg_commission_money_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.journal_id IS DISTINCT FROM OLD.journal_id THEN
        RAISE EXCEPTION 'commission_immutable';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_commission_money_immutable ON commissions;
CREATE TRIGGER trg_commission_money_immutable BEFORE UPDATE ON commissions
FOR EACH ROW EXECUTE FUNCTION trg_commission_money_immutable();

CREATE TABLE IF NOT EXISTS payouts (
    id           BIGSERIAL PRIMARY KEY,
    affiliate_id BIGINT NOT NULL REFERENCES affiliates(id),
    amount       BIGINT NOT NULL CHECK (amount > 0),
    currency     TEXT NOT NULL DEFAULT 'USD',
    method       TEXT NOT NULL DEFAULT 'manual',
    status       TEXT NOT NULL DEFAULT 'requested'
                    CHECK (status IN ('requested','approved','processing','paid','failed')),
    journal_id   BIGINT REFERENCES journal_entries(id),
    provider_ref TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);

CREATE OR REPLACE FUNCTION trg_payout_money_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.affiliate_id IS DISTINCT FROM OLD.affiliate_id THEN
        RAISE EXCEPTION 'payout_immutable';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payout_money_immutable ON payouts;
CREATE TRIGGER trg_payout_money_immutable BEFORE UPDATE ON payouts
FOR EACH ROW EXECUTE FUNCTION trg_payout_money_immutable();

CREATE TABLE IF NOT EXISTS payout_lines (
    payout_id     BIGINT NOT NULL REFERENCES payouts(id),
    commission_id BIGINT NOT NULL REFERENCES commissions(id),
    PRIMARY KEY (payout_id, commission_id)
);

-- A commission lives in at most ONE payout (INV-10; attack 10 pin — the
-- NOT EXISTS guard in CreateBatch is snapshot-blind between txs).
CREATE UNIQUE INDEX IF NOT EXISTS uq_payout_line_commission
    ON payout_lines (commission_id);

CREATE TABLE IF NOT EXISTS outbox_events (
    id           BIGSERIAL PRIMARY KEY,
    topic        TEXT NOT NULL,
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT
);
CREATE INDEX IF NOT EXISTS ix_outbox_undelivered ON outbox_events(id)
    WHERE delivered_at IS NULL;

-- Buyer email for fraud correlation (payment fingerprint) + operator
-- visibility. Idempotent.
ALTER TABLE commissions ADD COLUMN IF NOT EXISTS buyer_email TEXT NOT NULL DEFAULT '';
