// retention.go — the privacy retention sweep (NFR-7: clicks older than the
// window are deleted, aggregates preserved). Runs as a periodic ticker in
// the serve path and as a one-shot via the doctor's sibling; the 90d
// default is configurable per deployment.
package analytics

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// Retention prunes raw click rows beyond the window. Daily aggregates
// (daily_agg, token_stats_daily, uniq_daily) are RETAINED — they carry no
// personal data (ip_hash never leaves the clicks row's identity scope for
// uniques, which prunes with the same sweep).
type Retention struct {
	db     *sql.DB
	window time.Duration
}

// NewRetention builds the sweeper. window <= 0 defaults to 90d (the PRD's
// privacy commitment).
func NewRetention(db *sql.DB, window time.Duration) *Retention {
	if window <= 0 {
		window = 90 * 24 * time.Hour
	}
	return &Retention{db: db, window: window}
}

// Sweep prunes one batch and returns rows removed. Bounded batches keep
// the sweep lock-friendly on live SQLite writers.
func (r *Retention) Sweep(ctx context.Context) (int64, error) {
	cutoffDay := dayNum(time.Now().UTC().Add(-r.window))
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM clicks WHERE day < ? AND id IN (
			SELECT id FROM clicks WHERE day < ? LIMIT 5000
		)`, cutoffDay, cutoffDay)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// uniq_daily identity rows ride the same window (privacy: the unique
	// counters are day-scoped; old days carry no forward value either).
	if _, err := r.db.ExecContext(ctx, `DELETE FROM uniq_daily WHERE day < ?`, cutoffDay); err != nil {
		return n, err
	}
	return n, nil
}

// RunForever loops the sweep hourly until ctx ends (the serve-path wiring).
func (r *Retention) RunForever(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.Sweep(ctx)
			if err != nil {
				log.Printf("retention sweep: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("retention sweep: pruned %d clicks (> %s)", n, r.window)
			}
		}
	}
}

func dayNum(t time.Time) int64 {
	return int64(t.Year())*10000 + int64(t.Month())*100 + int64(t.Day())
}
