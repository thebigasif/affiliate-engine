package payouts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// ErrNotPaid: ReversePaid of a payout that is not 'paid'.
var ErrNotPaid = errors.New("payouts: payout is not paid")

// ErrReverseBlocked: a member commission of the payout is no longer
// 'paid' (refunded after payout: its clawback already moved the money),
// so the payout cannot be reversed as a whole; reconcile by hand.
var ErrReverseBlocked = errors.New("payouts: payout cannot be reversed")

// ReversalKey is the ledger idempotency key of a payout's reversing
// journal.
func ReversalKey(payoutID int64) string { return fmt.Sprintf("payout-reversal:%d", payoutID) }

// ReversalLines are the reversing journal's lines: SettleLines negated
// (the amount back from clearing onto the affiliate's payable).
func ReversalLines(affiliateID int64, currency string, amount int64) []ledger.Line {
	return []ledger.Line{
		{Account: ledger.Payable(affiliateID, currency), Amount: -amount},
		{Account: ledger.Clearing(currency), Amount: amount},
	}
}

// ReversePaid reverses a PAID payout whose money did not reach the
// affiliate after all (FR-119 revert of a manual payment, MO-16), in ONE
// transaction on the runner's settle ledger:
//
//   - the payout moves to the sanctioned reversed state ('failed' with
//     reversed_at and the reason; the status machine allows paid ->
//     failed only so), its lines are copied to payout_reversal_lines and
//     released;
//   - its commissions move from 'paid' back to 'approved' (allowed only
//     for a reversed payout's former members), so the next batch pays
//     them again; netted clawback lines are released too and net again;
//   - when the payout's settle journal exists, the reversing journal
//     (CLEARING -> PAYABLE, ReversalKey) posts, so the payable owes the
//     amount again and the ledger still sums to zero;
//   - an audit_log row records the actor and the reason (the status
//     triggers add the transitions).
//
// Refused: a payout that is not paid (ErrNotPaid), an empty reason, or a
// member commission no longer 'paid' (ErrReverseBlocked). Callers decide
// which payouts may be reversed (manual methods only) and who may.
func (r *Runner) ReversePaid(ctx context.Context, payoutID int64, reason, actor string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("payouts: a reason is required to reverse a payout")
	}
	if actor == "" {
		actor = "system"
	}
	unlock, err := r.lockPayout(ctx, payoutID)
	if err != nil {
		return err
	}
	defer unlock()
	_, run := r.settlePost()
	if run == nil {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("payouts: reverse %d: begin: %w", payoutID, err)
		}
		defer tx.Rollback()
		if err := r.reverseTx(ctx, tx, "", nil, payoutID, reason, actor); err != nil {
			return err
		}
		return tx.Commit()
	}
	return run.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		return r.reverseTx(ctx, tx, tx.ForUpdate(), tx, payoutID, reason, actor)
	})
}

func (r *Runner) reverseTx(ctx context.Context, q settleQ, forUpdate string, lt *ledger.Tx, payoutID int64, reason, actor string) error {
	var status, currency string
	var affiliateID, amount int64
	if err := q.QueryRowContext(ctx,
		`SELECT status, affiliate_id, currency, amount FROM payouts WHERE id = ?`+forUpdate, payoutID).
		Scan(&status, &affiliateID, &currency, &amount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: payout %d not found", ErrNotPaid, payoutID)
		}
		return fmt.Errorf("payouts: reverse lookup %d: %w", payoutID, err)
	}
	if status != "paid" {
		return fmt.Errorf("%w: payout %d is %s", ErrNotPaid, payoutID, status)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, status, amount FROM commissions
		WHERE id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)
		ORDER BY id`+forUpdate, payoutID)
	if err != nil {
		return fmt.Errorf("payouts: reverse members: %w", err)
	}
	var blocked []string
	for rows.Next() {
		var id, amt int64
		var st string
		if err := rows.Scan(&id, &st, &amt); err != nil {
			_ = rows.Close()
			return err
		}
		if amt > 0 && st != "paid" {
			blocked = append(blocked, fmt.Sprintf("%d (%s)", id, st))
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(blocked) > 0 {
		return fmt.Errorf("%w: payout %d commissions no longer paid: %s", ErrReverseBlocked, payoutID, strings.Join(blocked, ", "))
	}
	now := r.now().UTC().Format(time.RFC3339)
	if _, err := q.ExecContext(ctx, `
		INSERT INTO payout_reversal_lines (payout_id, commission_id)
		SELECT payout_id, commission_id FROM payout_lines WHERE payout_id = ?`, payoutID); err != nil {
		return fmt.Errorf("payouts: reverse history: %w", err)
	}
	res, err := q.ExecContext(ctx, `
		UPDATE payouts SET status = 'failed', reversed_at = ?, attention = ?
		WHERE id = ? AND status = 'paid'`, now, "reversed: "+reason, payoutID)
	if err != nil {
		return fmt.Errorf("payouts: reverse payout: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("payouts: payout %d left paid during its reversal", payoutID)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM payout_lines WHERE payout_id = ?`, payoutID); err != nil {
		return fmt.Errorf("payouts: reverse release lines: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE commissions SET status = 'approved', paid_at = NULL
		WHERE id IN (SELECT commission_id FROM payout_reversal_lines WHERE payout_id = ?)
		  AND status = 'paid' AND amount > 0`, payoutID); err != nil {
		return fmt.Errorf("payouts: reverse commissions: %w", err)
	}
	if lt != nil {
		var settled int64
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM journal_entries WHERE idem_key = ?`, SettleKey(payoutID)).Scan(&settled); err != nil {
			return fmt.Errorf("payouts: reverse journal lookup: %w", err)
		}
		if settled > 0 {
			if _, err := lt.Post(ctx, ReversalKey(payoutID), "payout_reversal",
				fmt.Sprintf("payout %d reversed: %s", payoutID, reason), r.now().UTC(),
				ReversalLines(affiliateID, currency, amount)); err != nil {
				return fmt.Errorf("payouts: reversing journal: %w", err)
			}
			if err := reverseTax(ctx, q, lt, payoutID, r.now().UTC()); err != nil {
				return err
			}
		}
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO audit_log (actor, table_name, row_id, action, detail)
		VALUES (?, 'payouts', ?, 'reverse', ?)`, actor, payoutID, reason); err != nil {
		return fmt.Errorf("payouts: reverse audit: %w", err)
	}
	return nil
}
