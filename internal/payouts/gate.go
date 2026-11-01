package payouts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/moneylock"
)

// GateKind names why a staged payout may not be paid right now.
type GateKind string

const (
	// GateHold: a member commission has an open fraud hold (F5).
	GateHold GateKind = "hold"
	// GateTaxDoc: the affiliate has a tax document pending or rejected (FR-17).
	GateTaxDoc GateKind = "tax_doc"
	// GatePaused: the affiliate is frozen (by a fraud rule or the operator)
	// or removed. Retryable: resuming them makes the payout payable again.
	GatePaused GateKind = "paused"
	// GateNotApproved: a member commission left 'approved' after staging.
	GateNotApproved GateKind = "not_approved"
	// GateClawback: a clawback landed after staging and is not netted.
	GateClawback GateKind = "clawback"
	// GatePayee: the host's ExportPayee refused the payee (a payout
	// details change still cooling, a method this rail does not pay).
	GatePayee GateKind = "payee"
)

// GateBlock is a reason a payout may not be paid.
type GateBlock struct {
	Kind   GateKind
	Reason string
}

// ErrNeedsAttention reports an Approve of an exported payout that the gate
// now blocks. The payout was in a PayPal/Wise file that may already have
// been paid, so it is not restaged (that would pay its commissions again):
// it stays 'requested' with payouts.attention set until the operator
// clears the cause and approves again.
var ErrNeedsAttention = errors.New("payouts: exported payout needs attention")

// gate is the single payment gate: Approve runs it before any money moves
// and ClaimForExport runs it before a payout goes into a manual-rail file.
// Read-only. nil block = payable now. Callers hold the payout's and its
// commissions' money locks.
func (r *Runner) gate(ctx context.Context, payoutID, affiliateID int64, currency string) (*GateBlock, error) {
	// F5 + attack-10 pin: holds placed AFTER staging block payment.
	var openHolds int64
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commission_holds ch
		JOIN payout_lines pl ON pl.commission_id = ch.commission_id
		WHERE pl.payout_id = ? AND ch.resolved_at IS NULL`, payoutID).
		Scan(&openHolds); err != nil {
		return nil, fmt.Errorf("payouts: hold check: %w", err)
	}
	if openHolds > 0 {
		return &GateBlock{GateHold, fmt.Sprintf("%d open fraud hold(s)", openHolds)}, nil
	}
	// A frozen (paused) or removed affiliate is not paid, including a payout
	// staged before they were paused.
	var affStatus string
	if err := r.db.QueryRowContext(ctx, `SELECT status FROM affiliates WHERE id = ?`, affiliateID).
		Scan(&affStatus); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("payouts: affiliate status gate: %w", err)
	}
	if affStatus != "" && affStatus != "active" {
		return &GateBlock{GatePaused, fmt.Sprintf("affiliate %d is paused (%s)", affiliateID, affStatus)}, nil
	}
	// FR-17 tax-doc gate re-check: an operator can record a blocking tax
	// document (or the affiliate can upload one, which lands 'pending')
	// AFTER the batch was staged.
	var blockedDoc string
	err := r.db.QueryRowContext(ctx, `
		SELECT doc_type FROM tax_documents
		WHERE affiliate_id = ? AND status IN ('pending','rejected')
		ORDER BY id LIMIT 1`, affiliateID).Scan(&blockedDoc)
	if err == nil {
		return &GateBlock{GateTaxDoc, fmt.Sprintf("affiliate %d tax document %q blocks payout", affiliateID, blockedDoc)}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("payouts: taxdoc gate: %w", err)
	}
	// Attack-10 pin: a member commission that left 'approved' after
	// staging (fraud uphold / full refund won the race) must not be paid.
	var notApproved int64
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM payout_lines pl
		JOIN commissions c ON c.id = pl.commission_id
		WHERE pl.payout_id = ? AND c.amount > 0 AND c.status != 'approved'`, payoutID).
		Scan(&notApproved); err != nil {
		return nil, fmt.Errorf("payouts: status gate: %w", err)
	}
	if notApproved > 0 {
		return &GateBlock{GateNotApproved, fmt.Sprintf("%d commission(s) left 'approved' after staging", notApproved)}, nil
	}
	// Never pay more than is owed: a clawback that landed after staging (a
	// partial refund on a member, or a refund on an already-paid commission
	// of this affiliate+currency) shrank the payable below the staged
	// amount.
	var unnetted int64
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commissions k
		JOIN commissions p ON p.id = k.parent_commission_id
		WHERE k.affiliate_id = ? AND k.currency = ? AND k.amount < 0
		  AND NOT EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = k.id)
		  AND (p.status IN ('paid','clawback')
		       OR EXISTS (SELECT 1 FROM payout_lines pl2
		                  WHERE pl2.payout_id = ? AND pl2.commission_id = p.id))`,
		affiliateID, currency, payoutID).Scan(&unnetted); err != nil {
		return nil, fmt.Errorf("payouts: clawback gate: %w", err)
	}
	if unnetted > 0 {
		return &GateBlock{GateClawback, fmt.Sprintf("%d clawback(s) landed after staging", unnetted)}, nil
	}
	return nil, nil
}

// lockPayout takes the payout's money lock plus every member commission's
// lock (the same set Approve takes). Returns the unlock func.
func (r *Runner) lockPayout(ctx context.Context, payoutID int64) (func(), error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT commission_id FROM payout_lines WHERE payout_id = ?`, payoutID)
	if err != nil {
		return nil, fmt.Errorf("payouts: lines: %w", err)
	}
	var names []string
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		names = append(names, moneylock.Commission(cid))
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names = append(names, moneylock.Payout(payoutID))
	return moneylock.LockAllIn(ctx, names), nil
}

// ExportedPayout is one payout claimed into a manual-rail file.
type ExportedPayout struct {
	PayoutID    int64
	AffiliateID int64
	Email       string
	Amount      int64
	Currency    string
}

// ExportResult is what ClaimForExport claimed and what it left out.
type ExportResult struct {
	Batch   string
	Payouts []ExportedPayout
	Skipped int // requested payouts of this method the gate blocks now
	// Blocked names each skipped payout and why (len(Blocked) == Skipped).
	Blocked []BlockedPayout
}

// BlockedPayout is a payout the gate or ExportPayee left out of a file.
type BlockedPayout struct {
	PayoutID int64
	Block    GateBlock
}

// ClaimForExport selects the payouts a PayPal or Wise file may list and
// claims them, so the file and the ledger never disagree:
//
//   - only 'requested' payouts whose method is the rail ("paypal" or
//     "wise"), never exported before;
//   - each one passes Gate, the exact check Approve runs (open fraud
//     holds, tax-doc pending/rejected, member commissions still
//     'approved', no clawback landed after staging); a blocked one is
//     left out and counted in Skipped (Approve restages or retries it as
//     usual, since it was never in a file);
//   - under the payout's and its commissions' money locks, exported_at
//     and export_batch are stamped with a compare-and-set, so a second
//     export (or a concurrent one) can never list it again.
//
// The operator pays the file, then approves each payout, which re-checks
// the gate and settles the ledger without running a rail.
func (r *Runner) ClaimForExport(ctx context.Context, method string) (ExportResult, error) {
	now := r.now().UTC()
	return r.export(ctx, method, fmt.Sprintf("%s-%s", method, now.Format("20060102T150405.000000000Z")), now, true, nil)
}

// ClaimForExportOnly is ClaimForExport limited to the given payouts:
// the same rail filter, gate, ExportPayee and compare-and-set claim, for
// a file the operator already holds (a preview). Payouts not in ids are
// never looked at; ids that are not requested, unexported payouts of
// this method are ignored.
func (r *Runner) ClaimForExportOnly(ctx context.Context, method string, ids []int64) (ExportResult, error) {
	now := r.now().UTC()
	only := make(map[int64]bool, len(ids))
	for _, id := range ids {
		only[id] = true
	}
	return r.export(ctx, method, fmt.Sprintf("%s-%s", method, now.Format("20060102T150405.000000000Z")), now, true, only)
}

// PreviewExport lists what ClaimForExport would claim now (the same
// rail filter, the same gate and ExportPayee) without claiming
// anything: exported_at and export_batch stay unset, so the payouts
// stay listable and payable. Batch is empty.
func (r *Runner) PreviewExport(ctx context.Context, method string) (ExportResult, error) {
	return r.export(ctx, method, "", r.now().UTC(), false, nil)
}

// export runs the claim (or, claim=false, the check) over the
// candidates; only, when not nil, limits them to its ids.
func (r *Runner) export(ctx context.Context, method, batch string, now time.Time, claim bool, only map[int64]bool) (ExportResult, error) {
	res := ExportResult{Batch: batch}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM payouts
		WHERE status = 'requested' AND method = ? AND exported_at IS NULL
		ORDER BY id`, method)
	if err != nil {
		return res, fmt.Errorf("payouts: export candidates: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return res, err
		}
		if only != nil && !only[id] {
			continue
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, id := range ids {
		p, block, err := r.claimOne(ctx, id, method, res.Batch, now, claim)
		if err != nil {
			return res, err
		}
		if block != nil {
			res.Skipped++
			res.Blocked = append(res.Blocked, BlockedPayout{PayoutID: id, Block: *block})
			continue
		}
		if p != nil {
			res.Payouts = append(res.Payouts, *p)
		}
	}
	return res, nil
}

// claimOne claims one payout under its money locks (claim=false only
// checks it). A non-nil block: the gate or ExportPayee refuses it. p=nil
// with no block: someone else claimed or approved it first.
func (r *Runner) claimOne(ctx context.Context, id int64, method, batch string, now time.Time, claim bool) (*ExportedPayout, *GateBlock, error) {
	unlock, err := r.lockPayout(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	var p ExportedPayout
	var status, m string
	var exported bool
	if err := r.db.QueryRowContext(ctx, `
		SELECT p.id, p.affiliate_id, COALESCE(a.email, ''), p.amount, p.currency, p.status, p.method,
		       CASE WHEN p.exported_at IS NULL THEN 0 ELSE 1 END
		FROM payouts p JOIN affiliates a ON a.id = p.affiliate_id
		WHERE p.id = ?`, id).
		Scan(&p.PayoutID, &p.AffiliateID, &p.Email, &p.Amount, &p.Currency, &status, &m, &exported); err != nil {
		return nil, nil, fmt.Errorf("payouts: export lookup %d: %w", id, err)
	}
	if status != "requested" || m != method || exported {
		return nil, nil, nil
	}
	block, err := r.gate(ctx, id, p.AffiliateID, p.Currency)
	if err != nil || block != nil {
		return nil, block, err
	}
	if r.ExportPayee != nil {
		payee, block, err := r.ExportPayee(ctx, id, p.AffiliateID, method, p.Email)
		if err != nil || block != nil {
			return nil, block, err
		}
		p.Email = payee
	}
	if !claim {
		return &p, nil, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE payouts SET exported_at = ?, export_batch = ?
		WHERE id = ? AND status = 'requested' AND method = ? AND exported_at IS NULL`,
		now.Format(time.RFC3339Nano), batch, id, method)
	if err != nil {
		return nil, nil, fmt.Errorf("payouts: claim %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil, nil
	}
	return &p, nil, nil
}
