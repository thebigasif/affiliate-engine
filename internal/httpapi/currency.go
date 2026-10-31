package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Per-currency money in API responses. Amounts in different currencies
// are never summed: a total is a list with one entry per currency
// (money.Total). The single-number fields that predate this keep their
// names and report one currency, never a mix (see primaryCurrency).

// sumsByCurrency runs a (currency, sum) GROUP BY query into a map.
func sumsByCurrency(ctx context.Context, db *sql.DB, q string, args ...any) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c sql.NullString
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[money.Normalize(c.String)] += v
	}
	return out, rows.Err()
}

// payableLike is the LIKE pattern (ESCAPE '!') matching an affiliate's
// payable accounts in every currency: AFFILIATE_PAYABLE:<id>:<CCY>.
func payableLike(affID int64) string {
	return fmt.Sprintf("AFFILIATE!_PAYABLE:%d:%%", affID)
}

// sortedCurrencies returns a per-currency map's keys in code order.
func sortedCurrencies[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// primaryCurrency is the one currency a legacy single-number field
// reports: the program currency when there is money in it (or none at
// all), else the only currency there is, else the program currency (the
// other currencies are in the per-currency lists).
func primaryCurrency[V any](m map[string]V, program string) string {
	if _, ok := m[program]; ok || len(m) != 1 {
		return program
	}
	for c := range m {
		return c
	}
	return program
}

// ruleCurrency validates an optional currency for a fixed amount or a tier
// threshold: "" stores NULL (= the program currency); otherwise a
// 3-letter ISO 4217 code, upper-cased.
func ruleCurrency(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	c := money.Normalize(s)
	if !money.Valid(c) {
		return nil, errors.New("currency must be a 3-letter ISO 4217 code")
	}
	return c, nil
}

// moneyByAffiliate returns each affiliate's commission totals by status,
// one entry per currency (sorted), for the given ids.
func (a *api) moneyByAffiliate(ctx context.Context, ids []int64) (map[int64][]currencyStatusTotals, error) {
	out := map[int64][]currencyStatusTotals{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// #nosec G202 -- only "?" placeholders are concatenated.
	rows, err := a.db.QueryContext(ctx, `
		SELECT affiliate_id, currency, status, COALESCE(SUM(amount),0) FROM commissions
		WHERE affiliate_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)
		GROUP BY affiliate_id, currency, status`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[int64]map[string]map[string]int64{}
	for rows.Next() {
		var id, v int64
		var c, s string
		if err := rows.Scan(&id, &c, &s, &v); err != nil {
			return nil, err
		}
		c = money.Normalize(c)
		if by[id] == nil {
			by[id] = map[string]map[string]int64{}
		}
		if by[id][c] == nil {
			by[id][c] = map[string]int64{}
		}
		by[id][c][s] += v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, m := range by {
		for _, c := range sortedCurrencies(m) {
			out[id] = append(out[id], currencyStatusTotals{Currency: c, ByStatusCents: m[c]})
		}
	}
	return out, nil
}

// totalsList turns a per-currency map into the API's sorted list.
func totalsList(m map[string]int64) []money.Total {
	return money.Totals(m).List()
}
