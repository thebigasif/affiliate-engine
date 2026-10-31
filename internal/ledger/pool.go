// Pooled posting for the Shopify multi-tenant server (BUILD-GUIDE 6.4
// refactor 6, BUILD-PLAN T-007).
//
// The self-host Poster pins ONE connection that every money write
// serializes through, under a process-wide mutex. One server serving
// thousands of shop schemas cannot run money that way (PRD 12.4 item 2):
// a blocked post in shop A would stall shop B. PooledPoster takes a
// connection from the pool per transaction and serializes per shop with
// pg_advisory_xact_lock keyed by (shop, entity), so two shops' money
// transactions never contend on the same lock (AC: a blocked post in
// shop A does not delay shop B's maturation by more than 1 s).
//
// It satisfies the commissions.Poster interface unchanged. Inside Run,
// everything runs with the shop's search_path set via SET LOCAL, so the
// transaction — and only that transaction — sees the shop's schema and
// the shared control schema. Lock keys are namespaced with the shop, so
// the same entity id in two shops maps to two different advisory locks.
//
// Postgres only: the Shopify server never runs on SQLite.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

// PooledPoster posts money through pool connections, one transaction per
// post, serialized per shop with advisory locks.
type PooledPoster struct {
	pool     *sql.DB
	schema   string // shop schema, e.g. "shop_3f9a"; control is always appended
	now      func() time.Time
	lockShop string // advisory-lock namespace for this shop
}

// NewPooledPoster binds a pooled poster to a shop's schema. schema must
// match ^shop_[a-z0-9_]{1,48}$ (validated by the tenancy layer, T-006);
// every transaction sets search_path to schema, control.
func NewPooledPoster(pool *sql.DB, schema string) *PooledPoster {
	return &PooledPoster{pool: pool, schema: schema, now: time.Now, lockShop: "ledger:" + schema + ":"}
}

// SetClock overrides the clock (tests).
func (p *PooledPoster) SetClock(now func() time.Time) { p.now = now }

// Run executes fn inside ONE transaction on a pool connection with the
// shop's search_path set via SET LOCAL. Journal posts and domain DML
// through the Tx commit atomically or not at all.
//
// fn may be run more than once: a transaction that loses a race it can
// only recover from by starting over (concurrent same-key journal, PG
// deadlock or serialization failure) is retried, at most runAttempts
// times. fn must derive everything from what it reads inside the
// transaction, and set any results it hands out on every run.
//
// Nested use joins (the same contract as Poster.Run): Run, Post,
// PostClawback and AccountBalance called with the ctx of an open Run of
// a PooledPoster on the same pool and shop run inside that transaction.
// A fraud screen that fails therefore rolls back the credit journal it
// screens, and one purchase holds one pool connection, not two.
func (p *PooledPoster) Run(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error {
	if t := txFromPool(ctx, p.pool, p.lockShop); t != nil {
		return fn(ctx, t)
	}
	var err error
	for attempt := 0; attempt < runAttempts; attempt++ {
		err = p.runOnce(ctx, fn)
		if err == nil || !(errors.Is(err, errIdemRace) || db.IsTxConflict(err)) {
			return err
		}
	}
	return err
}

func (p *PooledPoster) runOnce(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error {
	conn, err := p.pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("ledger: pooled conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	stx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin: %w", err)
	}
	defer func() { _ = stx.Rollback() }()

	// Tenant scope: this transaction — and only this transaction — sees
	// the shop's schema plus control. Committed or rolled back, the
	// connection returns to the pool with the server default path.
	if _, err := stx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path = %s, control", p.schema)); err != nil {
		return fmt.Errorf("ledger: set tenant path: %w", err)
	}

	t := &Tx{tx: stx, lc: nil, now: p.now, pg: true, lockNS: p.lockShop, pool: p.pool}
	if err := fn(WithTx(ctx, t), t); err != nil {
		return err
	}
	if err := stx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit: %w", err)
	}
	return nil
}

// Post inserts a journal atomically; idemKey makes it idempotent (see
// Poster.Post).
func (p *PooledPoster) Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error) {
	return p.post(ctx, idemKey, source, description, effective, lines, false)
}

// PostClawback is Post without the payable overdraft guard (see
// Poster.PostClawback).
func (p *PooledPoster) PostClawback(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error) {
	return p.post(ctx, idemKey, source, description, effective, lines, true)
}

func (p *PooledPoster) post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line, allowPayableDebit bool) (int64, error) {
	if err := checkLines(lines); err != nil {
		return 0, err
	}
	if t := txFromPool(ctx, p.pool, p.lockShop); t != nil {
		// Joined: the post commits or rolls back with the open Run.
		return t.post(ctx, idemKey, source, description, lines, allowPayableDebit)
	}
	var id int64
	err := p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		var err error
		id, err = tx.post(ctx, idemKey, source, description, lines, allowPayableDebit)
		return err
	})
	if errors.Is(err, ErrDuplicate) {
		// An existing journal under idemKey: its id and ErrDuplicate
		// (callers treat it as success-no-op), like the pinned poster.
		return id, ErrDuplicate
	}
	if errors.Is(err, errIdemRace) {
		// Every retry lost the same-key race; surface the duplicate
		// contract. The retry's pre-check normally finds the winner first.
		return id, fmt.Errorf("%w: %v", ErrDuplicate, err)
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AccountBalance returns the derived balance for an account in the
// shop's schema.
func (p *PooledPoster) AccountBalance(ctx context.Context, code string) (int64, error) {
	var bal int64
	err := p.Run(ctx, func(ctx context.Context, tx *Tx) error { // joins an open Run
		return tx.tx.QueryRowContext(ctx, balanceSQL, code).Scan(&bal)
	})
	if err != nil {
		return 0, fmt.Errorf("ledger: balance %q: %w", code, err)
	}
	return bal, nil
}

// GlobalIntegrity returns the sum of ALL ledger entries in the shop's
// schema. Must always be 0.
func (p *PooledPoster) GlobalIntegrity(ctx context.Context) (int64, error) {
	var sum int64
	err := p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		return tx.tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM ledger_entries`).Scan(&sum)
	})
	if err != nil {
		return 0, fmt.Errorf("ledger: integrity: %w", err)
	}
	return sum, nil
}

// EnsureAccounts creates ledger accounts idempotently in the shop's
// schema (see Poster.EnsureAccounts).
func (p *PooledPoster) EnsureAccounts(ctx context.Context, codes map[string]string) error {
	return p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		for code, kind := range codes {
			cur, ok := AccountCurrency(code)
			if !ok {
				return fmt.Errorf("ledger: ensure account %q: not a per-currency account code", code)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ledger_accounts (code, kind, currency) VALUES (?, ?, ?)
				ON CONFLICT(code) DO NOTHING`, code, kind, cur); err != nil {
				return fmt.Errorf("ledger: ensure account %q: %w", code, err)
			}
		}
		return nil
	})
}
