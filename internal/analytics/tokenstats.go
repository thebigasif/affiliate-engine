// Token-dimension analytics: watermark-driven rollup of clicks into
// token_stats_daily (day x affiliate x campaign x dim x value) and the
// placement drill API. The Kuma pattern from DATA-ARCHITECTURE: on-write
// for head dims is served by daily_agg; token dims roll on a watermark
// cursor because the value cardinality is unbounded.
package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// TokenStats rolls clicks into the token-dimension table.
type TokenStats struct{ db *sql.DB }

// NewTokenStats builds one.
func NewTokenStats(db *sql.DB) *TokenStats { return &TokenStats{db} }

// Rollup processes clicks newer than the cursor: unpivots the three token
// columns into (dim, value) rows and upserts counters. Idempotent per day:
// each day's clicks are fully recomputed from raw (delete+insert inside
// the tx), so re-runs and late clicks are both handled exactly. Clicks are
// the currency-less (”) rows; the per-currency money rows ApplyConversion
// writes are kept.
func (t *TokenStats) Rollup(ctx context.Context) (rows int64, err error) {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Days with clicks beyond the cursor's last fully-rolled day.
	rowsQ, err := tx.QueryContext(ctx, `
		SELECT DISTINCT day FROM clicks
		WHERE day > COALESCE((SELECT last_rollup_day FROM token_stats_cursor WHERE id=1), 0)
		   OR day = (SELECT last_rollup_day FROM token_stats_cursor WHERE id=1)
		ORDER BY day`)
	if err != nil {
		return 0, err
	}
	var days []int64
	for rowsQ.Next() {
		var d int64
		if err := rowsQ.Scan(&d); err != nil {
			_ = rowsQ.Close()
			return 0, err
		}
		days = append(days, d)
	}
	_ = rowsQ.Close()
	if len(days) == 0 {
		return 0, nil
	}

	for _, day := range days {
		// Full recompute of the day (idempotent: delete then insert).
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM token_stats_daily WHERE day = ? AND currency = ''`, day); err != nil {
			return rows, err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO token_stats_daily
			    (day, affiliate_id, campaign_id, dim, value, clicks, conversions, commission_cents)
			SELECT day, affiliate_id, campaign_id, 'utm_source', utm_source,
			       COUNT(*), 0, 0
			FROM clicks WHERE day = ? AND exclude_stats = 0 AND utm_source != ''
			GROUP BY day, affiliate_id, campaign_id, utm_source
			UNION ALL
			SELECT day, affiliate_id, campaign_id, 'subid', subid, COUNT(*), 0, 0
			FROM clicks WHERE day = ? AND exclude_stats = 0 AND subid != ''
			GROUP BY day, affiliate_id, campaign_id, subid
			UNION ALL
			SELECT day, affiliate_id, campaign_id, 'placement', placement, COUNT(*), 0, 0
			FROM clicks WHERE day = ? AND exclude_stats = 0 AND placement != ''
			GROUP BY day, affiliate_id, campaign_id, placement`,
			day, day, day)
		if err != nil {
			return rows, fmt.Errorf("tokenstats: day %d: %w", day, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rows += n
		}
	}

	// Advance the cursor to the newest day seen (fully recomputed).
	if _, err := tx.ExecContext(ctx, `
		UPDATE token_stats_cursor
		SET last_click_ts = ?, last_rollup_day = ?
		WHERE id = 1`,
		time.Now().UTC().Format(time.RFC3339Nano), days[len(days)-1]); err != nil {
		return rows, err
	}
	return rows, tx.Commit()
}

// DrillRow is one (dim, value) performance slice. Clicks and conversions
// count every currency; CommissionCents is in Currency only, and
// Commissions lists each currency's commission (never added up).
type DrillRow struct {
	Day             int64         `json:"day"`
	Dim             string        `json:"dim"`
	Value           string        `json:"value"`
	Clicks          int64         `json:"clicks"`
	Conversions     int64         `json:"conversions"`
	CommissionCents int64         `json:"commission_cents"`
	Currency        string        `json:"currency"`
	Commissions     []money.Total `json:"commissions"`
}

// Drill returns token-dimension performance, newest first.
// dim: "utm_source" | "subid" | "placement" | "" (all dims). currency is
// the one CommissionCents reports (the program currency).
func (t *TokenStats) Drill(ctx context.Context, affiliateID int64, dim string, fromDay, toDay int64, currency string, limit int) ([]DrillRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	currency = money.Normalize(currency)
	where := ` WHERE affiliate_id = ? AND day >= ? AND day <= ?`
	args := []any{affiliateID, fromDay, toDay}
	if dim != "" {
		where += ` AND dim = ?`
		args = append(args, dim)
	}
	rows, err := t.db.QueryContext(ctx, `
		SELECT day, dim, value, SUM(clicks), SUM(conversions),
		       SUM(CASE WHEN currency = ? THEN commission_cents ELSE 0 END)
		FROM token_stats_daily`+where+`
		GROUP BY day, dim, value
		ORDER BY day DESC, SUM(clicks) DESC, dim, value LIMIT ?`,
		append(append([]any{currency}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	type key struct {
		day        int64
		dim, value string
	}
	var out []DrillRow
	at := map[key]int{}
	for rows.Next() {
		r := DrillRow{Currency: currency, Commissions: []money.Total{}}
		if err := rows.Scan(&r.Day, &r.Dim, &r.Value, &r.Clicks, &r.Conversions, &r.CommissionCents); err != nil {
			_ = rows.Close()
			return nil, err
		}
		at[key{r.Day, r.Dim, r.Value}] = len(out)
		out = append(out, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	// Each slice's commission per currency, from the money rows.
	moneyRows, err := t.db.QueryContext(ctx, `
		SELECT day, dim, value, currency, SUM(commission_cents)
		FROM token_stats_daily`+where+` AND currency <> ''
		GROUP BY day, dim, value, currency`, args...)
	if err != nil {
		return nil, err
	}
	defer moneyRows.Close()
	per := map[int]money.Totals{}
	for moneyRows.Next() {
		var k key
		var cur string
		var v int64
		if err := moneyRows.Scan(&k.day, &k.dim, &k.value, &cur, &v); err != nil {
			return nil, err
		}
		if i, ok := at[k]; ok {
			if per[i] == nil {
				per[i] = money.Totals{}
			}
			per[i].Add(cur, v)
		}
	}
	if err := moneyRows.Err(); err != nil {
		return nil, err
	}
	for i, tot := range per {
		out[i].Commissions = tot.List()
	}
	return out, nil
}

// ApplyConversion credits a conversion + commission to the day's token
// slices for the affiliate (best-effort: attribution-level precision comes
// from linking commission -> click tokens; day-level is the pragmatic v1).
// The money goes to each slice's row in the conversion's currency.
func (t *TokenStats) ApplyConversion(ctx context.Context, day, affiliateID, campaignID int64, currency string, commissionCents int64) error {
	_, err := t.db.ExecContext(ctx, `
		INSERT INTO token_stats_daily
		    (day, affiliate_id, campaign_id, dim, value, currency, clicks, conversions, commission_cents)
		SELECT day, affiliate_id, campaign_id, dim, value, ?, 0, 1, ?
		FROM token_stats_daily
		WHERE day = ? AND affiliate_id = ? AND campaign_id = ? AND currency = ''
		ON CONFLICT (day, affiliate_id, campaign_id, dim, value, currency)
		DO UPDATE SET conversions = token_stats_daily.conversions + 1,
		              commission_cents = token_stats_daily.commission_cents + excluded.commission_cents`,
		money.Normalize(currency), commissionCents, day, affiliateID, campaignID)
	return err
}
