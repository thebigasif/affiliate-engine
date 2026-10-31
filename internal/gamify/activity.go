package gamify

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// TickActivity derives XP and badges from click-day aggregates (daily_agg):
// the traffic/streak half of the gamification wedge. Every grant dedupes on
// a unique key, so ticking (or replaying) is idempotent — same contract as
// OnCommission.
//
//   - clicks_block XP: XPPer100Clicks for each full 100 lifetime clicks
//     (block N has source_ref "N"; blocks are granted in order, so a
//     COUNT(*) resume pointer never double-awards)
//   - streak_30 badge: >= 30 consecutive active days (a day with clicks in
//     daily_agg), ending today or yesterday (today not being over yet must
//     not read as a broken streak)
//   - traffic_master badge: >= 10,000 lifetime clicks
func (e *Engine) TickActivity(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx,
		`SELECT DISTINCT affiliate_id FROM daily_agg WHERE clicks > 0`)
	if err != nil {
		return fmt.Errorf("gamify: activity scan: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := e.affiliateActivity(ctx, id, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

// affiliateActivity evaluates one affiliate's traffic-derived rewards.
func (e *Engine) affiliateActivity(ctx context.Context, affiliateID int64, now time.Time) error {
	// Lifetime clicks from the aggregate substrate (exclude_stats already
	// applied upstream; dashboards read the same numbers).
	var totalClicks int64
	if err := e.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE affiliate_id = ? AND clicks > 0`,
		affiliateID).Scan(&totalClicks); err != nil {
		return err
	}

	// 1) Click-block XP: resume from however many blocks are already granted.
	var granted int64
	if err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM xp_events WHERE affiliate_id = ? AND reason = 'clicks_block'`,
		affiliateID).Scan(&granted); err != nil {
		return err
	}
	for b := granted; b < totalClicks/100; b++ {
		if _, err := e.GrantXP(ctx, affiliateID, "clicks_block",
			strconv.FormatInt(b, 10), XPPer100Clicks); err != nil {
			return err
		}
	}

	// 2) Streak badge.
	if s := e.activeStreakDays(ctx, affiliateID, now); s >= 30 {
		if _, err := e.AwardBadge(ctx, affiliateID, "streak_30"); err != nil {
			return err
		}
	}

	// 3) Traffic badge.
	if totalClicks >= 10000 {
		if _, err := e.AwardBadge(ctx, affiliateID, "traffic_master"); err != nil {
			return err
		}
	}
	return nil
}

// activeStreakDays counts consecutive active days (daily_agg rows with
// clicks > 0) ending today — or yesterday, since the current day is not yet
// over. Each probe is a PK lookup; the walk stops at the first gap and is
// capped as a sanity bound.
func (e *Engine) activeStreakDays(ctx context.Context, affiliateID int64, now time.Time) int64 {
	day := dayInt(now)
	var streak int64
	var n int64
	err := e.db.QueryRowContext(ctx,
		`SELECT clicks FROM daily_agg WHERE affiliate_id = ? AND day = ? AND clicks > 0`,
		affiliateID, day).Scan(&n)
	if err == sql.ErrNoRows {
		day = prevDay(day) // today simply has no clicks yet
	} else if err != nil {
		return 0
	}
	for d := day; ; d = prevDay(d) {
		var c int64
		err := e.db.QueryRowContext(ctx,
			`SELECT clicks FROM daily_agg WHERE affiliate_id = ? AND day = ? AND clicks > 0`,
			affiliateID, d).Scan(&c)
		if err != nil {
			return streak // gap (or read error) ends the run
		}
		streak++
		if streak >= 1000 {
			return streak
		}
	}
}

// dayInt converts a UTC time to the YYYYMMDD integer used by daily_agg.
func dayInt(t time.Time) int64 {
	n, _ := strconv.ParseInt(t.Format("20060102"), 10, 64)
	return n
}

// prevDay steps a YYYYMMDD integer back one calendar day (never arithmetic
// minus-one: month/year boundaries would produce 20250100-style ghosts).
func prevDay(d int64) int64 {
	t, err := time.Parse("20060102", fmt.Sprintf("%08d", d))
	if err != nil {
		return d
	}
	return dayInt(t.AddDate(0, 0, -1))
}
