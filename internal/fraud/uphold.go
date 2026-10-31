// Production fraud uphold: cancel the commission and claw the money back
// through the ledger. This is the ONE implementation — httpapi and any
// future workers call it; the old httpapi-local copy and the deadlocked
// fraud.Review are gone (attack 1d).
//
// Money safety (attacks 1b/4/5): the claw journal, the status CAS and the
// hold resolution commit in ONE ledger transaction (Poster.Run), with the
// commission row locked (FOR UPDATE on Postgres) before its state is read.
// A failure anywhere changes nothing, so the decision stays retryable; a
// maturation, refund or payout settle of the same commission — in this
// process or another replica — either finished first or waits. (The old
// shape posted the claw, then cancelled in a separate transaction, and
// "reversed" the claw with a second post when it lost a status race.)
package fraud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/moneylock"
)

// Poster is the ledger dependency (*ledger.Poster satisfies it). Uphold
// runs in Run and posts the claw through Post with Run's ctx (joining the
// transaction).
type Poster interface {
	Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []ledger.Line) (int64, error)
	Run(ctx context.Context, fn func(ctx context.Context, tx *ledger.Tx) error) error
}

// ErrTerminalState: the commission is already cancelled/clawed (uphold is
// idempotent-reject), or paid (use the refund flow — see below).
var ErrTerminalState = errors.New("fraud: commission in terminal state")

// ErrAlreadyPaid: the rail already moved the money. Cancelling here would
// resurrect a paid commission; the operator recovers through ProcessRefund
// (paid -> clawback posts from payable/payout_clearing per its own rules).
var ErrAlreadyPaid = errors.New("fraud: commission already paid; use the refund flow")

// ErrPayoutInFlight: the commission is in a payout another replica is
// executing (status 'processing'). Its rail may already have paid it, so
// it cannot be clawed as unpaid; retry once that payout settles (it then
// reports ErrAlreadyPaid) or fails.
var ErrPayoutInFlight = errors.New("fraud: commission is in a payout being executed; retry after it settles")

// Uphold cancels the commission and posts the clawback.
//
// States: pending -> cancelled (claw from HOLD), approved -> cancelled
// (claw from payable). paid -> REJECTED with ErrAlreadyPaid: after the rail
// executed, fraud cancellation would fight the payout journal; the refund
// flow already handles post-payout reversal.
//
// db is unused since the uphold moved onto the ledger transaction; it is
// kept so callers need not change.
func Uphold(ctx context.Context, db *sql.DB, poster Poster, commissionID int64, note string) error {
	_ = db
	// In-process fast path: queue same-commission rivals here.
	unlock := moneylock.LockIn(ctx, moneylock.Commission(commissionID))
	defer unlock()
	return poster.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		return upholdTx(ctx, tx, poster, commissionID, note)
	})
}

func upholdTx(ctx context.Context, tx *ledger.Tx, poster Poster, commissionID int64, note string) error {
	var amount, affiliateID int64
	var status, currency string
	if err := tx.QueryRowContext(ctx,
		`SELECT amount, affiliate_id, status, currency FROM commissions WHERE id = ?`+tx.ForUpdate(), commissionID).
		Scan(&amount, &affiliateID, &status, &currency); err != nil {
		return err
	}

	// Claw only what is still live: an earlier partial refund already
	// clawed part of this commission (its negative child rows), out of
	// HOLD while pending or out of the payable once approved. Clawing the
	// full amount again took that part twice (over-drained HOLD, or an
	// affiliate charged twice for one refund).
	var clawed int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM commissions
		WHERE parent_commission_id = ? AND amount < 0`, commissionID).Scan(&clawed); err != nil {
		return err
	}
	remaining := amount - clawed

	var fromAccount string
	switch status {
	case "pending":
		fromAccount = ledger.Hold(currency)
	case "approved":
		fromAccount = ledger.Payable(affiliateID, currency)
		// A payout executing this commission in another replica holds no
		// lock across its rail call; the in-process moneylock covers it
		// within one process only.
		var inFlight int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM payout_lines pl JOIN payouts p ON p.id = pl.payout_id
			WHERE pl.commission_id = ? AND p.status = 'processing'`, commissionID).Scan(&inFlight); err != nil {
			return err
		}
		if inFlight > 0 {
			return ErrPayoutInFlight
		}
	case "cancelled", "clawback":
		return ErrTerminalState // idempotent-reject; nothing to do
	case "paid":
		return ErrAlreadyPaid
	default:
		return fmt.Errorf("%w: %s", ErrTerminalState, status)
	}

	// 1) Claw. ErrDuplicate = a pre-atomic attempt's claw landed and its
	// cancel did not: the money is moved, finish the cancel. The one
	// exception is a claw that release reversed after losing its status
	// race (fraudclaw-revert): the key is spent, so that case needs an
	// operator rather than a silent "already clawed".
	if remaining > 0 {
		clawKey := fmt.Sprintf("fraudclaw:%d", commissionID)
		_, err := poster.Post(ctx, clawKey, "fraud_clawback", note,
			time.Now().UTC(), []ledger.Line{
				{Account: fromAccount, Amount: remaining},
				{Account: ledger.Expense(currency), Amount: -remaining},
			})
		if errors.Is(err, ledger.ErrDuplicate) {
			var reversed int
			if qerr := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM journal_entries WHERE idem_key = ?`,
				fmt.Sprintf("fraudclaw-revert:%d", commissionID)).Scan(&reversed); qerr != nil {
				return qerr
			}
			if reversed > 0 {
				return fmt.Errorf("fraud: commission %d: an earlier uphold's clawback was reversed after a status race; claw it by hand (operator attention required)", commissionID)
			}
			err = nil
		}
		if err != nil {
			return fmt.Errorf("fraud: clawback post failed (nothing changed, hold left open): %w", err)
		}
	}

	// 2) Cancel + resolve the hold. CHECKED CAS: the row lock makes the
	// read stable; the guard stays as defence.
	res, err := tx.ExecContext(ctx,
		`UPDATE commissions SET status='cancelled' WHERE id = ? AND status = ?`,
		commissionID, status)
	if err != nil {
		return fmt.Errorf("fraud: cancel %d (nothing changed): %w", commissionID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: status changed under uphold (was %s)", ErrTerminalState, status)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE commission_holds SET resolved_at = ?, resolution = 'upheld', note = ?
		 WHERE commission_id = ? AND resolved_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), note, commissionID); err != nil {
		return fmt.Errorf("fraud: resolve holds of %d (nothing changed): %w", commissionID, err)
	}
	return nil
}
