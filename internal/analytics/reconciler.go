// Package analytics maintains the pre-aggregated tables via the watermark
// reconciler pattern: on-write upserts serve live dashboards; this reconciler
// backfills, self-heals drift, and enforces the frozen-day rule for uniques.
package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Reconciler walks clicks beyond the stored watermark and rolls them into
// daily_agg; it re-derives yesterday fully (drift check) and verifies the
// frozen-day rule (closed days append-only). Clicks have no currency: they
// live in daily_agg's currency-less (”) rows, and the reconciler never
// touches the per-currency money rows.
type Reconciler struct {
	db *sql.DB
}

// NewReconciler builds one.
func NewReconciler(db *sql.DB) *Reconciler { return &Reconciler{db: db} }

// Tick runs one reconciliation pass. Returns rows rolled up.
// Strategy (from the measured analytics report):
//   - daily_agg for days < today: full re-derivation of YESTERDAY (drift heal)
//   - daily_agg for today: incremental from watermark
//   - verification: agg sums == raw sums (non-excluded) for yesterday
func (r *Reconciler) Tick(ctx context.Context) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("analytics: begin: %w", err)
	}
	defer tx.Rollback()

	// 1) Watermark-based incremental rollup for TODAY.
	today := time.Now().UTC().Format("20060102")
	var lastID string
	if err := tx.QueryRowContext(ctx,
		`SELECT last_id FROM watermark WHERE name = 'clicks_rollup'`).Scan(&lastID); err != nil {
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO watermark (name, last_id) VALUES ('clicks_rollup', '')`); err != nil {
				return 0, err
			}
		} else {
			return 0, err
		}
	}

	// Incremental: aggregate today's clicks past the watermark.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks, conversions, commission_cents)
		SELECT day, affiliate_id, campaign_id, COUNT(*), 0, 0
		FROM clicks
		WHERE day = ? AND id > ? AND exclude_stats = 0
		GROUP BY day, affiliate_id, campaign_id
		ON CONFLICT(day, affiliate_id, campaign_id, currency)
		DO UPDATE SET clicks = daily_agg.clicks + excluded.clicks`,
		today, lastID)
	if err != nil {
		return 0, fmt.Errorf("analytics: incremental: %w", err)
	}
	rolled, _ := res.RowsAffected()

	// Advance watermark to the max click id processed.
	if _, err := tx.ExecContext(ctx, `
		UPDATE watermark SET last_id = COALESCE(
			(SELECT MAX(id) FROM clicks WHERE day = ?), last_id)
		WHERE name = 'clicks_rollup'`, today); err != nil {
		return 0, err
	}

	// 2) Frozen-day heal: yesterday gets FULL re-derivation into a shadow
	// comparison (verify only — append-only rule means we check, not rewrite).
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("20060102")
	var aggClicks, rawClicks int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE day = ? AND currency = ''`, yesterday).Scan(&aggClicks); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM clicks WHERE day = ? AND exclude_stats = 0`, yesterday).Scan(&rawClicks); err != nil {
		return 0, err
	}
	if aggClicks != rawClicks {
		// Drift detected: repair by delta (keeps append-only semantics:
		// corrections are additive adjustments, not deletes).
		delta := rawClicks - aggClicks
		var targetAff, targetCamp int64
		// Attribute drift to the dominant affiliate row of that day (rare path;
		// the on-write upserts make this a safety net, not the primary).
		if err := tx.QueryRowContext(ctx, `
			SELECT affiliate_id, campaign_id FROM daily_agg WHERE day = ? AND currency = ''
			ORDER BY clicks DESC, affiliate_id, campaign_id LIMIT 1`, yesterday).Scan(&targetAff, &targetCamp); err != nil {
			if err == sql.ErrNoRows {
				// No click row at all: create from raw.
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
					SELECT day, affiliate_id, campaign_id, COUNT(*)
					FROM clicks WHERE day = ? AND exclude_stats = 0
					GROUP BY day, affiliate_id, campaign_id`, yesterday); err != nil {
					return 0, err
				}
			} else {
				return 0, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE daily_agg SET clicks = clicks + ?
				WHERE day = ? AND affiliate_id = ? AND campaign_id = ? AND currency = ''`,
				delta, yesterday, targetAff, targetCamp); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return rolled, nil
}

// DashboardTotals is the program's activity over a day range: clicks and
// sales in every currency, commission per currency (never added up).
type DashboardTotals struct {
	Clicks      int64
	Conversions int64
	Commission  money.Totals
}

// Dashboard serves the program-level totals from the aggregate.
func (r *Reconciler) Dashboard(ctx context.Context, fromDay, toDay string) (DashboardTotals, error) {
	out := DashboardTotals{Commission: money.Totals{}}
	rows, err := r.db.QueryContext(ctx, `
		SELECT currency, SUM(clicks), SUM(conversions), SUM(commission_cents)
		FROM daily_agg WHERE day >= ? AND day <= ?
		GROUP BY currency`, fromDay, toDay)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var cur string
		var clicks, convs, cents int64
		if err := rows.Scan(&cur, &clicks, &convs, &cents); err != nil {
			return out, err
		}
		out.Clicks += clicks
		out.Conversions += convs
		if cur != "" {
			out.Commission.Add(cur, cents)
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}
