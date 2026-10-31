package mlm

import (
	"context"
	"fmt"
)

// RebuildClosure recomputes the ENTIRE mlm_closure table from the
// denormalized parent pointers on affiliates (sponsor_id, placement_parent_id
// + placement_leg). The drift-heal counterpart to the on-write maintenance
// (same contract as the analytics watermark reconciler: on-write is the
// primary, rebuild is the safety net).
//
// Returns rows written. Safe to run any time: rebuilds inside a transaction
// from source-of-truth columns, so a crash mid-rebuild changes nothing.
func (e *Engine) RebuildClosure(ctx context.Context) (int64, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM mlm_closure`); err != nil {
		return 0, err
	}

	// Sponsor tree edges: direct + transitive via recursive CTE.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level)
		WITH RECURSIVE up(id, root, depth) AS (
			SELECT id, sponsor_id, 1 FROM affiliates WHERE sponsor_id IS NOT NULL
			UNION ALL
			SELECT u.id, a.sponsor_id, u.depth + 1
			FROM up u JOIN affiliates a ON a.id = u.root
			WHERE a.sponsor_id IS NOT NULL
		)
		SELECT root, id, NULL, depth FROM up`); err != nil {
		return 0, fmt.Errorf("mlm: rebuild sponsor: %w", err)
	}

	// Placement tree: outer_leg = the leg of the FIRST hop from each ancestor
	// (the path's top edge below the ancestor). Recursive CTE carrying the
	// first-leg through the walk.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level)
		WITH RECURSIVE up(id, root, first_leg, depth) AS (
			SELECT id, placement_parent_id, placement_leg, 1
			FROM affiliates WHERE placement_parent_id IS NOT NULL
			UNION ALL
			-- Walking upward, the leg seen from each ancestor is the leg of
			-- its direct child on the path: REPLACE at each extension (the
			-- topmost hop's leg wins), not first-hop-preserved.
			SELECT u.id, a.placement_parent_id, a.placement_leg, u.depth + 1
			FROM up u JOIN affiliates a ON a.id = u.root
			WHERE a.placement_parent_id IS NOT NULL
		)
		SELECT root, id, first_leg, depth FROM up`); err != nil {
		return 0, fmt.Errorf("mlm: rebuild placement: %w", err)
	}

	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mlm_closure`).Scan(&n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// VerifyClosure reports drift between mlm_closure and a fresh computation.
// Returns (closureRows, expectedRows, error). A non-nil error means the
// closure table is inconsistent and RebuildClosure should run.
func (e *Engine) VerifyClosure(ctx context.Context) (have, want int64, err error) {
	// Count existing rows.
	if err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mlm_closure`).Scan(&have); err != nil {
		return 0, 0, err
	}
	// Count expected rows from the same recursive logic (sponsor + placement).
	if err := e.db.QueryRowContext(ctx, `
		WITH RECURSIVE up(id, root, depth) AS (
			SELECT id, sponsor_id, 1 FROM affiliates WHERE sponsor_id IS NOT NULL
			UNION ALL
			SELECT u.id, a.sponsor_id, u.depth + 1
			FROM up u JOIN affiliates a ON a.id = u.root
			WHERE a.sponsor_id IS NOT NULL
		), s AS (SELECT COUNT(*) c FROM (SELECT root, id FROM up))
		SELECT c FROM s`).Scan(&want); err != nil {
		return 0, 0, err
	}
	var placementRows int64
	if err := e.db.QueryRowContext(ctx, `
		WITH RECURSIVE up(id, root, depth) AS (
			SELECT id, placement_parent_id, 1 FROM affiliates WHERE placement_parent_id IS NOT NULL
			UNION ALL
			SELECT u.id, a.placement_parent_id, u.depth + 1
			FROM up u JOIN affiliates a ON a.id = u.root
			WHERE a.placement_parent_id IS NOT NULL
		)
		SELECT COUNT(*) FROM (SELECT root, id FROM up)`).Scan(&placementRows); err != nil {
		return 0, 0, err
	}
	want += placementRows
	if have != want {
		return have, want, fmt.Errorf("mlm: closure drift: have %d rows, computed %d — run RebuildClosure", have, want)
	}
	return have, want, nil
}
