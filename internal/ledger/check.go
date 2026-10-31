package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// The daily ledger balance check (PRD 13.4): a shop's books are read
// back and every invariant that must hold at rest is checked. A balanced
// ledger alone does not catch a duplicate credit (EXP-11), so the
// checks tie the journals to the commissions and payouts they record.

// Violation is one broken invariant.
type Violation struct {
	Check    string `json:"check"`
	Currency string `json:"currency,omitempty"`
	Detail   string `json:"detail"`
}

// CheckQuerier is the read surface the check needs.
type CheckQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Check runs the at-rest invariants and returns every violation (empty:
// the books are sound).
//
//   - currency_sum: each currency's entries sum to zero.
//   - journal_balanced: each journal's entries sum to zero.
//   - hold_matches_pending: HOLD:<CCY> carries exactly the pending
//     commissions net of their clawbacks (MO-11).
//   - paid_payout_settled: every paid payout has its settle journal
//     (payout:<id>), and the journal moves the payout's amount.
func Check(ctx context.Context, q CheckQuerier) ([]Violation, error) {
	var out []Violation
	totals, err := CurrencyIntegrity(ctx, q)
	if err != nil {
		return nil, err
	}
	curs := make([]string, 0, len(totals))
	for c := range totals {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	for _, c := range curs {
		if v := totals[c]; v != 0 {
			out = append(out, Violation{Check: "currency_sum", Currency: c, Detail: fmt.Sprintf("entries sum to %d", v)})
		}
	}

	rows, err := q.QueryContext(ctx, `
		SELECT j.idem_key, SUM(e.amount) FROM ledger_entries e JOIN journal_entries j ON j.id = e.journal_id
		GROUP BY j.id, j.idem_key HAVING SUM(e.amount) <> 0 ORDER BY j.id LIMIT 50`)
	if err != nil {
		return nil, fmt.Errorf("ledger: check journals: %w", err)
	}
	for rows.Next() {
		var key string
		var sum int64
		if err := rows.Scan(&key, &sum); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, Violation{Check: "journal_balanced", Detail: fmt.Sprintf("journal %s sums to %d", key, sum)})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.QueryContext(ctx, `
		WITH ledger_hold AS (
			SELECT a.currency AS cur, COALESCE(SUM(e.amount), 0) AS bal
			FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
			WHERE a.code LIKE 'HOLD:%' GROUP BY a.currency
		), pending AS (
			SELECT p.currency AS cur,
			       SUM(p.amount + COALESCE((SELECT SUM(c.amount) FROM commissions c WHERE c.parent_commission_id = p.id), 0)) AS net
			FROM commissions p WHERE p.status = 'pending' AND p.parent_commission_id IS NULL GROUP BY p.currency
		)
		SELECT COALESCE(h.cur, p.cur), COALESCE(h.bal, 0), COALESCE(p.net, 0)
		FROM ledger_hold h FULL OUTER JOIN pending p ON p.cur = h.cur
		WHERE COALESCE(h.bal, 0) <> -COALESCE(p.net, 0)
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("ledger: check hold: %w", err)
	}
	for rows.Next() {
		var cur string
		var bal, net int64
		if err := rows.Scan(&cur, &bal, &net); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, Violation{
			Check: "hold_matches_pending", Currency: cur,
			Detail: fmt.Sprintf("HOLD is %d but pending commissions net %d", bal, net),
		})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.QueryContext(ctx, `
		SELECT p.id, p.amount, COALESCE((
			SELECT SUM(e.amount) FROM journal_entries j JOIN ledger_entries e ON e.journal_id = j.id
			WHERE j.idem_key = 'payout:' || p.id AND e.amount > 0), -1)
		FROM payouts p WHERE p.status = 'paid'
		  AND COALESCE((SELECT SUM(e.amount) FROM journal_entries j JOIN ledger_entries e ON e.journal_id = j.id
		                WHERE j.idem_key = 'payout:' || p.id AND e.amount > 0), -1) <> p.amount
		ORDER BY p.id LIMIT 50`)
	if err != nil {
		return nil, fmt.Errorf("ledger: check payouts: %w", err)
	}
	for rows.Next() {
		var id, amount, moved int64
		if err := rows.Scan(&id, &amount, &moved); err != nil {
			_ = rows.Close()
			return nil, err
		}
		d := fmt.Sprintf("paid payout %d of %d has no settle journal", id, amount)
		if moved >= 0 {
			d = fmt.Sprintf("paid payout %d of %d settled %d", id, amount, moved)
		}
		out = append(out, Violation{Check: "paid_payout_settled", Detail: d})
	}
	_ = rows.Close()
	return out, rows.Err()
}
