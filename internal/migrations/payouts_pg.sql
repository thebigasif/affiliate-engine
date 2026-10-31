-- Wave 2 additions: payout pipeline tables (PostgreSQL dialect).

CREATE TABLE IF NOT EXISTS payout_runs (
    id          BIGSERIAL PRIMARY KEY,
    status      TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running','completed','failed')),
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    note        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS payout_run_lines (
    run_id      BIGINT NOT NULL REFERENCES payout_runs(id),
    payout_id   BIGINT NOT NULL REFERENCES payouts(id),
    PRIMARY KEY (run_id, payout_id)
);
