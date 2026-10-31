// Package gamify implements the affiliate-love wedge: XP, levels, badges,
// leaderboards. XP derives ONLY from money/ledger facts (commissions,
// clicks) — never free-floating counters. Every grant is exactly-once via
// the (affiliate, reason, source_ref) unique key.
//
// Design restraint per product direction: no confetti storms, one hero
// moment (level-up in the portal); badges are informational identity,
// not noise.
package gamify

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// Engine grants XP and badges.
type Engine struct{ db *sql.DB }

// New builds one.
func New(db *sql.DB) *Engine { return &Engine{db: db} }

// XP schedule (documented, tweakable via code constants until the admin
// API exposes it).
const (
	XPFirstCommission = 500
	XPPerConversion   = 100
	XPPer100Clicks    = 10
	XPMilestone1k     = 1000 // first $10.00 earned (1000 cents)
	XPMilestone10k    = 2500
	XPMilestone100k   = 10000
)

// Level curve: quadratic — level N requires N*N*100 total XP
// (L2=400, L5=2500, L10=10000). Slow start, meaningful mastery tail.
func LevelForXP(xp int64) int64 {
	var level int64 = 1
	for (level+1)*(level+1)*100 <= xp {
		level++
	}
	return level
}

// GrantXP awards XP exactly-once. reason+sourceRef form the dedup key.
// Returns granted=false when already awarded.
func (e *Engine) GrantXP(ctx context.Context, affiliateID int64, reason, sourceRef string, xp int64) (granted bool, err error) {
	if xp <= 0 {
		return false, nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO xp_events (affiliate_id, reason, xp, source_ref)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(affiliate_id, reason, source_ref) DO NOTHING`,
		affiliateID, reason, xp, sourceRef)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // already granted
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO xp_totals (affiliate_id, xp, level, updated_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(affiliate_id) DO UPDATE SET
		  xp = xp_totals.xp + excluded.xp, updated_at = excluded.updated_at`,
		affiliateID, xp, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RecomputeLevel refreshes the cached level from the XP total. Returns
// (level, leveledUp) — the portal's single hero moment fires on leveledUp.
func (e *Engine) RecomputeLevel(ctx context.Context, affiliateID int64) (int64, bool, error) {
	var xp int64
	var oldLevel int64
	err := e.db.QueryRowContext(ctx,
		`SELECT xp, level FROM xp_totals WHERE affiliate_id = ?`, affiliateID).Scan(&xp, &oldLevel)
	if err == sql.ErrNoRows {
		return 1, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	newLevel := LevelForXP(xp)
	if newLevel == oldLevel {
		return oldLevel, false, nil
	}
	if _, err := e.db.ExecContext(ctx,
		`UPDATE xp_totals SET level = ? WHERE affiliate_id = ?`, newLevel, affiliateID); err != nil {
		return 0, false, err
	}
	return newLevel, newLevel > oldLevel, nil
}

// AwardBadge grants a badge exactly-once.
func (e *Engine) AwardBadge(ctx context.Context, affiliateID int64, code string) (bool, error) {
	res, err := e.db.ExecContext(ctx, `
		INSERT INTO affiliate_badges (affiliate_id, badge_code)
		VALUES (?, ?)
		ON CONFLICT DO NOTHING`, affiliateID, code)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// OnCommission is the money-hook: called after a commission is credited.
// Idempotent — wired via the outbox, replays are no-ops.
func (e *Engine) OnCommission(ctx context.Context, affiliateID int64, commissionID, amountCents int64) error {
	// Clawback rows (amount <= 0) are money REMOVED — they must never mint
	// XP, badges, or milestones; leaderboards would inflate on refunds.
	if amountCents <= 0 {
		return nil
	}
	// First commission: badge + XP.
	if granted, err := e.GrantXP(ctx, affiliateID, "first_commission", "lifetime", XPFirstCommission); err != nil {
		return err
	} else if granted {
		if _, err := e.AwardBadge(ctx, affiliateID, "first_commission"); err != nil {
			return err
		}
	}
	// Per-conversion XP.
	if _, err := e.GrantXP(ctx, affiliateID, "conversion", fmt.Sprint(commissionID), XPPerConversion); err != nil {
		return err
	}
	// Milestone badges (lifetime earned, ledger-derived amounts). The
	// thresholds are minor units of the program currency; earnings in
	// other currencies do not count toward them (no FX).
	var lifetime int64
	if err := e.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount),0) FROM commissions
		WHERE affiliate_id = ? AND currency = ? AND status IN ('pending','approved','paid') AND amount > 0`,
		affiliateID, settings.Currency(ctx, e.db)).Scan(&lifetime); err != nil {
		return err
	}
	milestones := []struct {
		cents int64
		code  string
		xp    int64
	}{
		{1000, "earner_1k", XPMilestone1k},
		{10000, "earner_10k", XPMilestone10k},
		{100000, "earner_100k", XPMilestone100k},
	}
	for _, m := range milestones {
		if lifetime >= m.cents {
			if granted, err := e.GrantXP(ctx, affiliateID, "milestone_"+m.code, "lifetime", m.xp); err != nil {
				return err
			} else if granted {
				if _, err := e.AwardBadge(ctx, affiliateID, m.code); err != nil {
					return err
				}
			}
		}
	}
	_, _, err := e.RecomputeLevel(ctx, affiliateID)
	return err
}

// OnClickBatch is the traffic-hook: called with the distinct active days
// and lifetime clicks of an affiliate (the click committer / reconciler
// can invoke it on day boundaries). Awards streak + traffic badges.
func (e *Engine) OnClickStats(ctx context.Context, affiliateID int64, activeDays, lifetimeClicks int64) error {
	if activeDays >= 30 {
		if granted, err := e.GrantXP(ctx, affiliateID, "badge_streak_30", "lifetime", 500); err != nil {
			return err
		} else if granted {
			if _, err := e.AwardBadge(ctx, affiliateID, "streak_30"); err != nil {
				return err
			}
		}
	}
	if lifetimeClicks >= 10000 {
		if granted, err := e.GrantXP(ctx, affiliateID, "badge_traffic_master", "lifetime", 1000); err != nil {
			return err
		} else if granted {
			if _, err := e.AwardBadge(ctx, affiliateID, "traffic_master"); err != nil {
				return err
			}
		}
	}
	_, _, err := e.RecomputeLevel(ctx, affiliateID)
	return err
}

// SeedBadges inserts the default badge catalog (idempotent).
func (e *Engine) SeedBadges(ctx context.Context) error {
	catalog := []struct{ code, name, desc, icon string }{
		{"first_commission", "First Commission", "Credited your first commission", "🥇"},
		{"earner_1k", "First $10", "Lifetime earnings crossed $10", "🌱"},
		{"earner_10k", "Century Club", "Lifetime earnings crossed $100", "💯"},
		{"earner_100k", "Grand Earner", "Lifetime earnings crossed $1,000", "🏆"},
		{"streak_30", "Consistency", "30 active days", "🔥"},
		{"traffic_master", "Traffic Master", "10,000 lifetime clicks", "🚀"},
	}
	for _, b := range catalog {
		if _, err := e.db.ExecContext(ctx, `
			INSERT INTO badges (code, name, description, icon) VALUES (?, ?, ?, ?)
			ON CONFLICT(code) DO NOTHING`, b.code, b.name, b.desc, b.icon); err != nil {
			return err
		}
	}
	return nil
}

// Profile returns the portal view: xp, level, progress to next level, badges.
func (e *Engine) Profile(ctx context.Context, affiliateID int64) (map[string]any, error) {
	var xp, level int64
	err := e.db.QueryRowContext(ctx,
		`SELECT xp, level FROM xp_totals WHERE affiliate_id = ?`, affiliateID).Scan(&xp, &level)
	if err == sql.ErrNoRows {
		xp, level = 0, 1
	} else if err != nil {
		return nil, err
	}
	nextAt := (level + 1) * (level + 1) * 100
	prevAt := level * level * 100
	progress := int64(0)
	if nextAt > prevAt {
		progress = (xp - prevAt) * 100 / (nextAt - prevAt)
	}
	badges, err := e.Badges(ctx, affiliateID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"affiliate_id":     affiliateID,
		"xp":               xp,
		"level":            level,
		"next_level_xp":    nextAt,
		"progress_percent": progress,
		"badges":           badges,
	}, nil
}

// Badges lists earned badges.
func (e *Engine) Badges(ctx context.Context, affiliateID int64) ([]map[string]any, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT b.code, b.name, b.icon, ab.awarded_at
		FROM affiliate_badges ab JOIN badges b ON b.code = ab.badge_code
		WHERE ab.affiliate_id = ? ORDER BY ab.awarded_at`, affiliateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var code, name, icon, at string
		if err := rows.Scan(&code, &name, &icon, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"code": code, "name": name, "icon": icon, "awarded_at": at})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}

// Leaderboard returns top affiliates by XP (30 days of activity optional).
func (e *Engine) Leaderboard(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT t.affiliate_id,
		       CASE WHEN a.name IS NOT NULL AND a.name != '' THEN a.name
		            ELSE 'Affiliate #' || a.id END,
		       t.xp, t.level
		FROM xp_totals t JOIN affiliates a ON a.id = t.affiliate_id
		ORDER BY t.xp DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	rank := 0
	for rows.Next() {
		rank++
		var id, xp, level int64
		var name string
		if err := rows.Scan(&id, &name, &xp, &level); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"rank": rank, "affiliate_id": id, "name": name, "xp": xp, "level": level})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}
