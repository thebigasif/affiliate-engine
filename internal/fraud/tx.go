package fraud

import (
	"context"
	"database/sql"
	"fmt"
)

// Querier is what rules, verdict records and holds read and write
// through: the pool (*sql.DB), or the credit's transaction (*ledger.Tx)
// when screening runs inside it.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{}

// WithTx returns a ctx under which the engine's rules, verdict records and
// holds all go through tx instead of the pool. Screening evaluates a
// commission inside the transaction that credits it, so the verdict and
// its hold commit with the credit or not at all. The rules then see the
// uncommitted commission row, and on SQLite no write waits on the lock
// that transaction holds.
func WithTx(ctx context.Context, tx Querier) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// Conn returns the transaction ctx carries (WithTx), else db. Custom rules
// read through it like the built-in ones.
func Conn(ctx context.Context, db Querier) Querier {
	if tx, ok := ctx.Value(txKey{}).(Querier); ok && tx != nil {
		return tx
	}
	return db
}

func inTx(ctx context.Context) bool {
	tx, ok := ctx.Value(txKey{}).(Querier)
	return ok && tx != nil
}

// contained runs fn under a savepoint when ctx carries a transaction, and
// rolls back to it when fn fails. A failed statement aborts a whole
// Postgres transaction; contained keeps one broken rule from taking the
// credit, and the screening_error hold after it, down with it.
func contained(ctx context.Context, fn func() error) error {
	if !inTx(ctx) {
		return fn()
	}
	q := Conn(ctx, nil)
	if _, err := q.ExecContext(ctx, `SAVEPOINT fraud_step`); err != nil {
		return fmt.Errorf("fraud: savepoint: %w", err)
	}
	if err := fn(); err != nil {
		if _, rerr := q.ExecContext(ctx, `ROLLBACK TO SAVEPOINT fraud_step`); rerr != nil {
			return fmt.Errorf("%w (rollback to savepoint: %w)", err, rerr)
		}
		_, _ = q.ExecContext(ctx, `RELEASE SAVEPOINT fraud_step`)
		return err
	}
	if _, err := q.ExecContext(ctx, `RELEASE SAVEPOINT fraud_step`); err != nil {
		return fmt.Errorf("fraud: release savepoint: %w", err)
	}
	return nil
}
