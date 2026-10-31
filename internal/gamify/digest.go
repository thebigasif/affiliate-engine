// AI digest: natural-language program summaries generated from ledger-
// derived facts. Deterministic template v1 (no external LLM call — the
// digest must work self-hosted offline); the hosted tier later feeds the
// same facts to a model for richer prose.
package gamify

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/money"
)

// GenerateDigest derives the digest from money facts (daily_agg traffic,
// commissions, fraud queue, ledger integrity) for the last N days. Money
// is reported per currency, never summed across currencies.
func (e *Engine) GenerateDigest(ctx context.Context, days int) (string, error) {
	if days <= 0 {
		days = 7
	}
	since := time.Now().UTC().AddDate(0, 0, -days)
	var d int64
	for _, c := range since.Format("20060102") {
		d = d*10 + int64(c-'0')
	}

	var clicks, convs int64
	if err := e.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0)
		FROM daily_agg WHERE day >= ?`, d).Scan(&clicks, &convs); err != nil {
		return "", fmt.Errorf("daily_agg sums: %w", err)
	}

	credited, top, err := CreditedByCurrency(ctx, e.db, since)
	if err != nil {
		return "", err
	}

	var fraudOpen, holds int64
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fraud_verdicts WHERE resolved=FALSE`).Scan(&fraudOpen); err != nil {
		return "", fmt.Errorf("fraud verdict count: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&holds); err != nil {
		return "", fmt.Errorf("holds count: %w", err)
	}

	var newAffs int64
	if err := e.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM affiliates WHERE created_at > ?`,
		since.Format(time.RFC3339)).Scan(&newAffs); err != nil {
		return "", fmt.Errorf("new affiliates count: %w", err)
	}

	integrity, err := ledger.CurrencyIntegrity(ctx, e.db)
	if err != nil {
		return "", fmt.Errorf("ledger integrity sum: %w", err)
	}
	ledgerStatus := "balanced"
	if !ledger.Balanced(integrity) {
		ledgerStatus = fmt.Sprintf("IMBALANCED (%s)", integrity)
	}

	cr := float64(0)
	if clicks > 0 {
		cr = float64(convs) / float64(clicks) * 100
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Program digest — last %d days\n\n", days)
	fmt.Fprintf(&sb, "Traffic: %d clicks, %d conversions (%.2f%% CVR)\n",
		clicks, convs, cr)
	fmt.Fprintf(&sb, "Commissions credited: %s\n", credited)
	for _, t := range top {
		if t.Name != "" {
			fmt.Fprintf(&sb, "Top earner: %s (%s)\n", t.Name, money.Display(t.Minor, t.Currency))
		}
	}
	fmt.Fprintf(&sb, "New affiliates: %d\n", newAffs)
	fmt.Fprintf(&sb, "Fraud queue: %d open verdicts, %d held commissions\n", fraudOpen, holds)
	fmt.Fprintf(&sb, "Ledger: %s\n", ledgerStatus)
	if holds > 0 {
		sb.WriteString("\nAction: review the fraud queue — held commissions block payouts.\n")
	}
	return sb.String(), nil
}

// TopEarner is the affiliate credited the most in one currency.
type TopEarner struct {
	Currency    string
	AffiliateID int64
	Name        string // name, else email (portal sign-up data: escape before display)
	Minor       int64
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CreditedByCurrency returns the commissions credited since a time, per
// currency, and each currency's top earner (sorted by currency). Earners
// are ranked within a currency only: yen and cents do not compare.
func CreditedByCurrency(ctx context.Context, q rowsQuerier, since time.Time) (money.Totals, []TopEarner, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT c.currency, c.affiliate_id, COALESCE(NULLIF(a.name, ''), a.email), COALESCE(SUM(c.amount),0)
		FROM commissions c JOIN affiliates a ON a.id = c.affiliate_id
		WHERE c.created_at > ? AND c.amount > 0
		GROUP BY c.currency, c.affiliate_id, a.name, a.email`, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, nil, fmt.Errorf("credited: %w", err)
	}
	defer rows.Close()
	total := money.Totals{}
	best := map[string]TopEarner{}
	for rows.Next() {
		var t TopEarner
		if err := rows.Scan(&t.Currency, &t.AffiliateID, &t.Name, &t.Minor); err != nil {
			return nil, nil, fmt.Errorf("credited: %w", err)
		}
		t.Currency = money.Normalize(t.Currency)
		total.Add(t.Currency, t.Minor)
		if b, ok := best[t.Currency]; !ok || t.Minor > b.Minor || (t.Minor == b.Minor && t.AffiliateID < b.AffiliateID) {
			best[t.Currency] = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("credited: %w", err)
	}
	top := make([]TopEarner, 0, len(best))
	for _, t := range best {
		top = append(top, t)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Currency < top[j].Currency })
	return total, top, nil
}
