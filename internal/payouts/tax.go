package payouts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// Tax on payouts (FR-107). The merchant decides whether VAT or
// withholding applies; the host computes it per payout (Runner.Tax) and
// the engine moves the right amount and books it apart from commission:
//
//   - VAT on top: the rail sends gross + VAT. The settle journal moves
//     the gross off the payable as always; a second journal books the
//     VAT to EXPENSE:VAT:<CCY> against PAYOUT_CLEARING.
//   - Withholding: the rail sends gross - withheld. The second journal
//     moves the withheld part from PAYOUT_CLEARING to TAX_WITHHELD:<CCY>,
//     which the merchant remits (we never hold or remit tax).
//   - VAT included: the rail sends the gross; nothing more posts (the
//     invoice shows the VAT part).
//
// The commission journal never changes, so commission totals and 1099
// totals stay gross (FR-107 AC3).

// Tax modes the engine books.
const (
	TaxNone        = "none"
	TaxVATOnTop    = "vat_on_top"
	TaxVATIncluded = "vat_included"
	TaxWithholding = "withholding_pct"
)

// TaxSplit is one payout's tax: the mode, its rate in basis points and
// the tax amount in the payout's minor units.
type TaxSplit struct {
	Mode     string `json:"mode"`
	RateBP   int64  `json:"rateBp"`
	TaxMinor int64  `json:"taxMinor"`
}

// Send is what the rail moves for a gross payout amount.
func (t TaxSplit) Send(gross int64) int64 {
	switch t.Mode {
	case TaxVATOnTop:
		return gross + t.TaxMinor
	case TaxWithholding:
		return gross - t.TaxMinor
	}
	return gross
}

// Lines are the tax journal's lines (nil when nothing books).
func (t TaxSplit) Lines(currency string) []ledger.Line {
	if t.TaxMinor <= 0 {
		return nil
	}
	switch t.Mode {
	case TaxVATOnTop:
		return []ledger.Line{
			{Account: ledger.VATExpense(currency), Amount: t.TaxMinor},
			{Account: ledger.Clearing(currency), Amount: -t.TaxMinor},
		}
	case TaxWithholding:
		return []ledger.Line{
			{Account: ledger.Clearing(currency), Amount: t.TaxMinor},
			{Account: ledger.TaxWithheld(currency), Amount: -t.TaxMinor},
		}
	}
	return nil
}

// TaxFunc returns a payout's tax. It must return the same split every
// time it is asked about one payout (the host records the first answer),
// so a retried approve sends and books the same amounts.
type TaxFunc func(ctx context.Context, q SettleExec, payoutID, affiliateID int64, currency string, gross int64) (TaxSplit, error)

// TaxKey and TaxReversalKey are the tax journals' idempotency keys.
func TaxKey(payoutID int64) string { return fmt.Sprintf("payout-tax:%d", payoutID) }

func TaxReversalKey(payoutID int64) string { return fmt.Sprintf("payout-tax-reversal:%d", payoutID) }

// sendAmount is what the rail moves for the payout (its gross when no
// tax is wired).
func (r *Runner) sendAmount(ctx context.Context, payoutID, affiliateID int64, currency string, gross int64) (int64, error) {
	if r.Tax == nil {
		return gross, nil
	}
	t, err := r.Tax(ctx, r.db, payoutID, affiliateID, currency, gross)
	if err != nil {
		return 0, fmt.Errorf("payouts: tax for %d: %w", payoutID, err)
	}
	send := t.Send(gross)
	if send <= 0 {
		return 0, fmt.Errorf("payouts: tax for %d leaves nothing to send", payoutID)
	}
	return send, nil
}

// postTax books the payout's tax journal in the settle transaction.
func (r *Runner) postTax(ctx context.Context, q settleQ, payoutID, affiliateID int64, currency string, gross int64, now time.Time) error {
	lt, ok := q.(*ledger.Tx)
	if r.Tax == nil || !ok {
		return nil
	}
	t, err := r.Tax(ctx, q, payoutID, affiliateID, currency, gross)
	if err != nil {
		return fmt.Errorf("tax: %w", err)
	}
	lines := t.Lines(currency)
	if lines == nil {
		return nil
	}
	_, err = lt.Post(ctx, TaxKey(payoutID), "payout_tax", fmt.Sprintf("payout %d %s", payoutID, t.Mode), now, lines)
	if errors.Is(err, ledger.ErrDuplicate) {
		return nil
	}
	return err
}

// reverseTax negates a reversed payout's tax journal, when one posted.
func reverseTax(ctx context.Context, q settleQ, lt *ledger.Tx, payoutID int64, now time.Time) error {
	rows, err := q.QueryContext(ctx, `
		SELECT la.code, le.amount
		FROM journal_entries je
		JOIN ledger_entries le ON le.journal_id = je.id
		JOIN ledger_accounts la ON la.id = le.account_id
		WHERE je.idem_key = ? ORDER BY le.id`, TaxKey(payoutID))
	if err != nil {
		return fmt.Errorf("payouts: tax journal lookup: %w", err)
	}
	var lines []ledger.Line
	for rows.Next() {
		var l ledger.Line
		if err := rows.Scan(&l.Account, &l.Amount); err != nil {
			_ = rows.Close()
			return err
		}
		l.Amount = -l.Amount
		lines = append(lines, l)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(lines) == 0 {
		return err
	}
	if _, err := lt.Post(ctx, TaxReversalKey(payoutID), "payout_tax_reversal",
		fmt.Sprintf("payout %d tax reversed", payoutID), now, lines); err != nil && !errors.Is(err, ledger.ErrDuplicate) {
		return fmt.Errorf("payouts: reversing tax journal: %w", err)
	}
	return nil
}
