package importer

// compare.go — side-by-side mode. While both trackers run, the operator
// feeds the rival's latest export in daily and gets their totals next to
// ours for the same days. Modest on purpose: totals, not a reconciliation.
//
// Definitions (the same on both sides):
//   clicks       rival: rows (or the count column) of their click export;
//                ours: clicks rows, known bots excluded (exclude_stats = 0)
//   referrals    new paying customers. rival: referrals that converted
//                (a customer id, or a converted status); ours: customer
//                bindings made in the range, imported bindings excluded
//   commissions  positive-amount rows that are not voided/cancelled; the
//                amount is summed per currency

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CompareInput is the rival side of a compare.
type CompareInput struct {
	Referrals                               []Referral
	Commissions                             []Commission
	Clicks                                  []DayCount
	HasReferrals, HasCommissions, HasClicks bool
	From, To                                time.Time // inclusive UTC days
}

// CompareRow is one metric, both sides.
type CompareRow struct {
	Metric   string `json:"metric"`
	Currency string `json:"currency,omitempty"`
	Theirs   *int64 `json:"theirs"` // nil: not in their export
	Ours     int64  `json:"ours"`
	Diff     *int64 `json:"diff"` // ours - theirs
}

// CompareReport is the side-by-side result.
type CompareReport struct {
	Source string       `json:"source"`
	From   string       `json:"from"`
	To     string       `json:"to"`
	Rows   []CompareRow `json:"rows"`
	Notes  []string     `json:"notes,omitempty"`
}

// Compare totals the rival export and our data over the same days.
func Compare(ctx context.Context, db *sql.DB, src Source, in CompareInput) (*CompareReport, error) {
	from, to := dayOf(in.From), dayOf(in.To)
	if to.Before(from) {
		return nil, fmt.Errorf("importer: compare range ends before it starts")
	}
	end := to.AddDate(0, 0, 1) // exclusive
	within := func(t time.Time) bool { return !t.IsZero() && !t.Before(from) && t.Before(end) }
	rep := &CompareReport{Source: string(src), From: from.Format("2006-01-02"), To: to.Format("2006-01-02")}
	row := func(metric, cur string, has bool, theirs, ours int64) {
		r := CompareRow{Metric: metric, Currency: cur, Ours: ours}
		if has {
			t, d := theirs, ours-theirs
			r.Theirs, r.Diff = &t, &d
		}
		rep.Rows = append(rep.Rows, r)
	}

	// Clicks.
	var theirClicks int64
	for _, d := range in.Clicks {
		if within(d.Day) {
			theirClicks += d.Count
		}
	}
	var ourClicks int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM clicks WHERE exclude_stats = 0 AND day >= ? AND day <= ?`,
		dayInt(from), dayInt(to)).Scan(&ourClicks); err != nil {
		return nil, err
	}
	row("clicks", "", in.HasClicks, theirClicks, ourClicks)
	if !in.HasClicks {
		rep.Notes = append(rep.Notes, "clicks: no click export given (-clicks), ours shown alone")
	}

	// Referrals (new paying customers).
	var theirRefs, undated int64
	for _, r := range in.Referrals {
		if rejectedStatus(r.Status) || !(r.CustomerID != "" || convertedStatus(r.Status)) {
			continue
		}
		if r.At.IsZero() {
			undated++
			continue
		}
		if within(r.At) {
			theirRefs++
		}
	}
	if undated > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("referrals: %d converted rows had no readable date and were left out", undated))
	}
	ourRefs, err := countBetween(ctx, db, `SELECT bound_at FROM customers
		WHERE bound_method NOT LIKE 'import:%' AND bound_at >= ? AND bound_at < ?`, from, end)
	if err != nil {
		return nil, err
	}
	row("referrals", "", in.HasReferrals, theirRefs, int64(len(ourRefs)))

	// Commissions: count, then amount per currency.
	theirN, theirSum := int64(0), map[string]int64{}
	for _, c := range in.Commissions {
		if c.Amount <= 0 || voidStatus(c.Status) || !within(c.At) || !knownCurrency(c.Currency) {
			continue
		}
		theirN++
		theirSum[c.Currency] += c.Amount
	}
	ourN, ourSum := int64(0), map[string]int64{}
	rows, err := db.QueryContext(ctx, `SELECT created_at, amount, currency FROM commissions
		WHERE amount > 0 AND status != 'cancelled' AND created_at >= ? AND created_at < ?`,
		from.AddDate(0, 0, -1).Format(time.RFC3339), end.AddDate(0, 0, 1).Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at any
		var amt int64
		var cur string
		if err := rows.Scan(&at, &amt, &cur); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if t, ok := asTime(at); ok && within(t) {
			ourN++
			ourSum[strings.ToUpper(cur)] += amt
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	row("commissions", "", in.HasCommissions, theirN, ourN)
	curs := map[string]bool{}
	for c := range theirSum {
		curs[c] = true
	}
	for c := range ourSum {
		curs[c] = true
	}
	var cl []string
	for c := range curs {
		cl = append(cl, c)
	}
	sort.Strings(cl)
	for _, c := range cl {
		row("commission amount", c, in.HasCommissions, theirSum[c], ourSum[c])
	}
	if !in.HasReferrals {
		rep.Notes = append(rep.Notes, "referrals: no referrals export given (-referrals), ours shown alone")
	}
	if !in.HasCommissions {
		rep.Notes = append(rep.Notes, "commissions: no commissions export given (-commissions), ours shown alone")
	}
	rep.Notes = append(rep.Notes, "While both trackers run, pay each commission from one system only.")
	return rep, nil
}

// countBetween selects one time column (prefiltered a day wide on each
// side, since SQLite stores two text shapes) and keeps rows in [from, end).
func countBetween(ctx context.Context, db *sql.DB, q string, from, end time.Time) ([]time.Time, error) {
	rows, err := db.QueryContext(ctx, q, from.AddDate(0, 0, -1).Format(time.RFC3339), end.AddDate(0, 0, 1).Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if t, ok := asTime(v); ok && !t.Before(from) && t.Before(end) {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// asTime reads a timestamp column on either engine (PG: time.Time;
// SQLite: RFC3339 text or datetime('now') text).
func asTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x.UTC(), true
	case string:
		return parseWhen(x)
	case []byte:
		return parseWhen(string(x))
	}
	return time.Time{}, false
}

func dayInt(t time.Time) int64 {
	n, _ := strconv.ParseInt(t.Format("20060102"), 10, 64)
	return n
}

// convertedStatus: a referral that became a paying customer.
func convertedStatus(s string) bool {
	switch s {
	case "conversion", "converted", "customer", "active", "paid", "paying", "trialing":
		return true
	}
	return false
}
