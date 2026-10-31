// Package mlm implements the multi-tier engine: two explicit trees (sponsor
// for unilevel payouts, placement for binary pairing later), a closure table
// with outer-leg tagging, and the three-stage period pipeline
// (counts -> dollars -> ledger) with CAS idempotency.
//
// Clean-room from the guruperl failure-mode study: no FLOAT money, no
// silently-consumed BV, re-runs never double-pay (unique period x affiliate
// x level x source), clawbacks propagate.
package mlm

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/bpmath"
	"github.com/thebigasif/affiliate-engine/internal/dbutil"
	"github.com/thebigasif/affiliate-engine/internal/money"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// Engine maintains trees and runs periods.
type Engine struct {
	db *sql.DB
}

// New builds one.
func New(db *sql.DB) *Engine { return &Engine{db: db} }

// --- tree maintenance ---

// PlaceInSponsorTree sets affiliate `id`'s sponsor and maintains the closure
// table. Idempotent; refuses cycles (an ancestor can't become a descendant).
// Re-parenting moves the WHOLE subtree: every descendant's ancestor paths
// are re-derived (stale paths would corrupt BV counters and tier walks).
func (e *Engine) PlaceInSponsorTree(ctx context.Context, id, sponsorID int64) error {
	if id == sponsorID {
		return fmt.Errorf("mlm: self-sponsor")
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Cycle check: sponsor must not be a descendant of id (sponsor tree).
	var cyc int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mlm_closure
		WHERE ancestor_id = ? AND descendant_id = ? AND outer_leg IS NULL`,
		id, sponsorID).Scan(&cyc); err != nil {
		return err
	}
	if cyc > 0 {
		return fmt.Errorf("mlm: cycle: %d is an ancestor of %d", id, sponsorID)
	}

	// Stage the moving subtree (id + its sponsor descendants with depth)
	// before deleting. Temp table inside the tx: pinned to the tx's
	// connection on both engines (pooled conns never see another tx's copy;
	// the pre-DELETE makes stale rows impossible).
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS mlm_moved (descendant_id INTEGER NOT NULL PRIMARY KEY, depth INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("mlm: stage table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mlm_moved`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_moved (descendant_id, depth)
		SELECT descendant_id, level FROM mlm_closure
		WHERE ancestor_id = ? AND outer_leg IS NULL`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mlm_moved (descendant_id, depth) VALUES (?, 0)`, id); err != nil {
		return err
	}

	// Clear stale sponsor rows for the subtree: only edges whose ANCESTOR
	// lies outside the moved set (intra-subtree edges like 2->3 inside a
	// moving {2,3} are still true and must survive).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mlm_closure
		WHERE outer_leg IS NULL
		  AND descendant_id IN (SELECT descendant_id FROM mlm_moved)
		  AND ancestor_id NOT IN (SELECT descendant_id FROM mlm_moved)`); err != nil {
		return err
	}
	// Direct edges: sponsor -> each subtree member at depth+1. (WHERE true
	// is required even here: a bare ON CONFLICT after FROM is parsed as a
	// JOIN constraint by SQLite — same rule as the transitive-edge stmt.)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level)
		SELECT ?, descendant_id, NULL, depth + 1 FROM mlm_moved
		WHERE true
		ON CONFLICT DO NOTHING`, sponsorID); err != nil {
		return err
	}
	// Transitive edges: sponsor's sponsor-ancestors -> each subtree member
	// at (ancestor level + depth + 1). The cycle pre-check guarantees none
	// of these ancestors lie inside the moving subtree. (The WHERE clause
	// is required: SQLite cannot disambiguate a bare ON CONFLICT directly
	// after a JOIN's ON condition.)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level)
		SELECT s.ancestor_id, m.descendant_id, NULL, s.level + m.depth + 1
		FROM mlm_moved m
		JOIN mlm_closure s ON s.descendant_id = ? AND s.outer_leg IS NULL
		WHERE true
		ON CONFLICT DO NOTHING`, sponsorID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE mlm_moved`); err != nil {
		return err
	}
	// Record the direct parent on the affiliate row (denormalized for reads).
	if _, err := tx.ExecContext(ctx, `
		UPDATE affiliates SET sponsor_id = ? WHERE id = ?`, sponsorID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// PlaceInPlacementTree sets the binary placement with leg tagging ('L'/'R').
// The placement tree is separate: outer_leg rows never mix with sponsor rows.
func (e *Engine) PlaceInPlacementTree(ctx context.Context, id, parentID int64, leg string) error {
	if leg != "L" && leg != "R" {
		return fmt.Errorf("mlm: leg must be L or R")
	}
	if id == parentID {
		return fmt.Errorf("mlm: self-placement")
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Cycle check: parent must not be a placement-descendant of id
	// (pointer cycles would poison every later walk).
	var cyc int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mlm_closure
		WHERE ancestor_id = ? AND descendant_id = ? AND outer_leg IS NOT NULL`,
		id, parentID).Scan(&cyc); err != nil {
		return err
	}
	if cyc > 0 {
		return fmt.Errorf("mlm: cycle: %d is a placement ancestor of %d", id, parentID)
	}

	// One direct placement parent per node (either leg).
	var existing int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mlm_closure
		WHERE descendant_id = ? AND outer_leg IS NOT NULL AND level = 1`, id).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return fmt.Errorf("mlm: %d already has a placement parent", id)
	}
	// Occupancy: parent's chosen leg can hold at most one direct child.
	var occ int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mlm_closure
		WHERE ancestor_id = ? AND outer_leg = ? AND level = 1`, parentID, leg).Scan(&occ); err != nil {
		return err
	}
	if occ > 0 {
		return fmt.Errorf("mlm: %s leg of %d occupied", leg, parentID)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level)
		VALUES (?, ?, ?, 1)
		ON CONFLICT DO NOTHING`, parentID, id, leg); err != nil {
		return err
	}
	// Transitive edges: ancestor -> id at level+1. Outer-leg tagging rule:
	// the tag from ancestor A to descendant D is the leg of the path's FIRST
	// hop below A — i.e. the leg from A to its direct child on the path.
	// For id's parent chain, that is the (A -> parent) leg for every PLACEMENT
	// ancestor of parent (parent itself handled by the direct edge above).
	// The outer_leg IS NOT NULL filter is the tree separation invariant:
	// sponsor ancestors must never leak into the placement closure.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level)
		SELECT a.ancestor_id,
		       ?,
		       a.outer_leg,  -- (A -> parent) leg; NULL impossible: filtered
		       a.level + 1
		FROM mlm_closure a
		WHERE a.descendant_id = ? AND a.ancestor_id != ? AND a.outer_leg IS NOT NULL
		ON CONFLICT DO NOTHING`, id, parentID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE affiliates SET placement_parent_id = ?, placement_leg = ? WHERE id = ?`,
		parentID, leg, id); err != nil {
		return err
	}
	return tx.Commit()
}

// --- stage 1: counts (BV accumulation) ---

// RecordBV bumps personal BV for a sale and group BV for every sponsor-tree
// ancestor (on-write counters; the reconciler self-heals drift).
//
// Volume is one number per day, so it is counted in one currency: the
// program currency (program_settings.currency), like bonus tier volume.
// A sale in another currency adds no BV (nothing is converted), so ¥20,000
// never counts as 20,000 toward a rank threshold set in cents.
func (e *Engine) RecordBV(ctx context.Context, affiliateID int64, day int64, currency string, bv int64) error {
	if bv == 0 || money.Normalize(currency) != settings.Currency(ctx, e.db) {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_volume_daily (day, affiliate_id, bv_personal)
		VALUES (?, ?, ?)
		ON CONFLICT(day, affiliate_id) DO UPDATE SET
		  bv_personal = mlm_volume_daily.bv_personal + excluded.bv_personal`,
		day, affiliateID, bv); err != nil {
		return err
	}
	// Group BV up the sponsor tree (closure walk, no recursion in Go).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_volume_daily (day, affiliate_id, bv_group)
		SELECT ?, c.ancestor_id, ?
		FROM mlm_closure c
		WHERE c.descendant_id = ? AND c.outer_leg IS NULL
		ON CONFLICT(day, affiliate_id) DO UPDATE SET
		  bv_group = mlm_volume_daily.bv_group + excluded.bv_group`,
		day, bv, affiliateID); err != nil {
		return err
	}
	return tx.Commit()
}

// --- stage 2+3: dollars -> ledger (the period run) ---

// RunPeriod closes a period: walks commissions earned in the window, splits
// each commissionable amount across the sponsor upline per mlm_tier_rates,
// and records mlm_period_lines. CAS on the period row guarantees one run;
// the unique (period, affiliate, level, source) key makes re-runs no-ops.
func (e *Engine) RunPeriod(ctx context.Context, start, end time.Time) (lines int64, err error) {
	// SQLite read->write lock upgrades fail fast with SQLITE_BUSY when a
	// concurrent writer holds the lock (busy_timeout cannot help upgrades).
	// Retry the whole run; period idempotency makes that safe.
	err = dbutil.RetryBusy(4, 25*time.Millisecond, func() error {
		var ierr error
		lines, ierr = e.runPeriodOnce(ctx, start, end)
		return ierr
	})
	return lines, err
}

func (e *Engine) runPeriodOnce(ctx context.Context, start, end time.Time) (lines int64, err error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Find-or-create the period row.
	var periodID int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM mlm_payout_periods WHERE period_start = ? AND period_end = ?`,
		start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano)).Scan(&periodID)
	if err == sql.ErrNoRows {
		if ierr := tx.QueryRowContext(ctx, `
			INSERT INTO mlm_payout_periods (period_start, period_end, status)
			VALUES (?, ?, 'calculating') RETURNING id`,
			start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano)).Scan(&periodID); ierr != nil {
			return 0, ierr
		}
	} else if err != nil {
		return 0, err
	} else {
		// Existing period: only re-run if failed; open/calculating/paid refuse.
		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM mlm_payout_periods WHERE id = ?`, periodID).Scan(&status); err != nil {
			return 0, err
		}
		if status != "failed" {
			return 0, fmt.Errorf("mlm: period %d is %s (refusing re-run)", periodID, status)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mlm_payout_periods SET status='calculating' WHERE id = ?`, periodID); err != nil {
			return 0, err
		}
	}

	// Walk approved commissions in the window with their earner's upline.
	// created_at is RFC3339Nano on both engines (writers normalized; legacy
	// SQLite t.String() rows are healed by the sqlite.sql migration), so a
	// plain comparison is correct — no query-side replace() gymnastics
	// (which also break on PG, where created_at is a real timestamptz).
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.affiliate_id, c.amount, c.currency
		FROM commissions c
		WHERE c.status IN ('approved','paid')
		  AND c.created_at >= ? AND c.created_at < ?
		  AND c.amount > 0`,
		start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	type src struct {
		id, aff, amount int64
		currency        string
	}
	var sources []src
	for rows.Next() {
		var s src
		if err := rows.Scan(&s.id, &s.aff, &s.amount, &s.currency); err != nil {
			_ = rows.Close()
			return 0, err
		}
		sources = append(sources, s)
	}
	_ = rows.Close()

	for _, s := range sources {
		// Tier split: sponsor-tree ancestors by level, rate from mlm_tier_rates.
		// F2 + overflow fix: a source commission pays each (ancestor,
		// level) exactly ONCE, EVER — and the split cents are computed with
		// CHECKED math in Go. SQLite silently promotes int64 overflow in
		// '? * rate_bp / 10000' to REAL (float money, the cardinal sin).
		rows2, err := tx.QueryContext(ctx, `
			SELECT c.ancestor_id, c.level, t.rate_bp
			FROM mlm_closure c
			JOIN mlm_tier_rates t ON t.level = c.level AND t.campaign_id = 1
			WHERE c.descendant_id = ?
			  AND c.outer_leg IS NULL
			  AND NOT EXISTS (SELECT 1 FROM mlm_period_lines x
			                  WHERE x.affiliate_id = c.ancestor_id
			                    AND x.level = c.level
			                    AND x.source_commission_id = ?)`,
			s.aff, s.id)
		if err != nil {
			return lines, fmt.Errorf("mlm: split select %d: %w", s.id, err)
		}
		type splitRow struct {
			ancestor, level, rateBP int64
		}
		var splits []splitRow
		for rows2.Next() {
			var sr splitRow
			if err := rows2.Scan(&sr.ancestor, &sr.level, &sr.rateBP); err != nil {
				_ = rows2.Close()
				return lines, err
			}
			splits = append(splits, sr)
		}
		_ = rows2.Close()
		for _, sr := range splits {
			// The split is in the currency of the commission it splits.
			// Checked math via bpmath (128-bit exact). A negative rate or an
			// unrepresentable split REJECTS the run (the tx rolls back) —
			// never a saturated MaxInt64 line or a negative payout.
			if sr.rateBP < 0 {
				return lines, fmt.Errorf("mlm: negative tier rate %d bp at level %d", sr.rateBP, sr.level)
			}
			cents, ok := bpmath.MulDivBP(s.amount, sr.rateBP)
			if !ok {
				return lines, fmt.Errorf("mlm: split overflow: commission %d amount=%d rate_bp=%d",
					s.id, s.amount, sr.rateBP)
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO mlm_period_lines
				    (period_id, affiliate_id, level, commission_cents, rate_bp_used, source_commission_id, currency)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT DO NOTHING`,
				periodID, sr.ancestor, sr.level, cents, sr.rateBP, s.id, s.currency)
			if err != nil {
				return lines, fmt.Errorf("mlm: split commission %d: %w", s.id, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				lines += n
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE mlm_payout_periods SET status='paid' WHERE id = ?`, periodID); err != nil {
		return lines, err
	}
	return lines, tx.Commit()
}

// PeriodTotal returns the computed split for one affiliate (audit view),
// per currency: splits of a dollar and a yen commission are never added.
func (e *Engine) PeriodTotal(ctx context.Context, periodID, affiliateID int64) (money.Totals, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT currency, COALESCE(SUM(commission_cents), 0)
		FROM mlm_period_lines
		WHERE period_id = ? AND affiliate_id = ?
		GROUP BY currency`, periodID, affiliateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := money.Totals{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out.Add(c, v)
	}
	return out, rows.Err()
}
