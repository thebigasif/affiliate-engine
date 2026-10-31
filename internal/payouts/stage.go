package payouts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/money"
	"github.com/thebigasif/affiliate-engine/internal/moneylock"
)

// ErrNothingPayable: no whole commissions of the affiliate fit the
// requested amount (or nothing is payable at all, net of clawbacks).
var ErrNothingPayable = errors.New("payouts: nothing payable")

// Payable is what one affiliate can be paid now in one currency, in its
// minor units: approved commissions not in any payout, without an open
// hold, of an active affiliate with no blocking tax document, net of the
// clawbacks a batch would net against them (receivables). It is exactly
// what CreateBatch would put in that affiliate's payout (threshold 0),
// so a payout request or a wallet redeem never sees more than a batch
// would pay. 0 when nothing (or a negative net) is owed.
func (r *Runner) Payable(ctx context.Context, affiliateID int64, currency string) (int64, error) {
	planned, err := planScoped(ctx, r.db, Uniform(0), affiliateID, currency)
	if err != nil {
		return 0, err
	}
	if len(planned) == 0 {
		return 0, nil
	}
	return planned[0].Cents, nil
}

// Staged is one payout Stage created.
type Staged struct {
	PayoutID int64
	Amount   int64 // net, minor units; <= the requested maximum
}

// Stage claims, for one affiliate and currency, whole payable
// commissions worth at most maxMinor into a new 'requested' payout
// (MO-12, MO-13): the payout row and its payout_lines are written in tx,
// so the commissions are claimed — no batch, request or redeem can claim
// them again (uq_payout_line_commission) — when tx commits, and not at
// all if it rolls back. The caller's own rows (the payout request, the
// wallet code) go in the same tx.
//
// Selection: every clawback a batch would net is netted (receivables are
// always settled first), then commissions oldest first while the net
// stays within maxMinor. Whole commissions only (INV-10), so the amount
// may be below maxMinor. ErrNothingPayable when the net is not positive.
//
// The claim is serialized per (shop schema, affiliate, currency): an
// in-process lock plus, on Postgres, a transaction advisory lock, so two
// concurrent stages queue instead of failing on the unique index.
func (r *Runner) Stage(ctx context.Context, tx *sql.Tx, affiliateID int64, currency string, maxMinor int64, method string) (Staged, error) {
	currency = money.Normalize(currency)
	if maxMinor <= 0 {
		return Staged{}, fmt.Errorf("%w: requested %d", ErrNothingPayable, maxMinor)
	}
	if r.postgres(ctx) {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':payout-stage:' || ?, 0))`,
			fmt.Sprintf("%d:%s", affiliateID, currency)); err != nil {
			return Staged{}, fmt.Errorf("payouts: stage lock: %w", err)
		}
	}
	planned, err := planScoped(ctx, tx, Uniform(0), affiliateID, currency)
	if err != nil {
		return Staged{}, err
	}
	if len(planned) == 0 {
		return Staged{}, ErrNothingPayable
	}
	pp := planned[0]
	var lines []int64
	var net int64
	for i, amt := range pp.amounts {
		if amt < 0 {
			lines = append(lines, pp.lines[i])
			net += amt
		}
	}
	for i, amt := range pp.amounts {
		if amt > 0 && net+amt <= maxMinor {
			lines = append(lines, pp.lines[i])
			net += amt
		}
	}
	if net <= 0 {
		return Staged{}, fmt.Errorf("%w: no whole commissions fit %d", ErrNothingPayable, maxMinor)
	}
	var pid int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO payouts (affiliate_id, amount, currency, method, status)
		VALUES (?, ?, ?, ?, 'requested') RETURNING id`, affiliateID, net, currency, method).Scan(&pid); err != nil {
		return Staged{}, fmt.Errorf("payouts: stage payout: %w", err)
	}
	for _, comm := range lines {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO payout_lines (payout_id, commission_id) VALUES (?, ?)`, pid, comm); err != nil {
			return Staged{}, fmt.Errorf("payouts: stage line: %w", err)
		}
	}
	return Staged{PayoutID: pid, Amount: net}, nil
}

// StageLock is the in-process half of Stage's serialization; callers
// hold it around their whole stage transaction (begin to commit). It is
// named under ctx's shop scope (moneylock.WithScope), so affiliate 7 of
// one shop never waits on affiliate 7 of another, and every stage path
// of one shop shares the name.
func StageLock(ctx context.Context, affiliateID int64, currency string) func() {
	return moneylock.LockAllIn(ctx, []string{fmt.Sprintf("payout-stage:%d:%s", affiliateID, money.Normalize(currency))})
}

// postgres reports whether the runner's database is Postgres (cached).
func (r *Runner) postgres(ctx context.Context) bool {
	r.dialectOnce.Do(func() {
		var v string
		if err := r.db.QueryRowContext(ctx, `SELECT version()`).Scan(&v); err == nil {
			r.pg = strings.Contains(v, "PostgreSQL")
		}
	})
	return r.pg
}
