package mlm_test

import (
	"context"
	"testing"
)

// RB1: rebuild reproduces on-write closure exactly (sponsor + placement).
func TestRebuild_MatchesOnWrite(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	// Sponsor chain: 2<-1, 3<-2.
	e.PlaceInSponsorTree(ctx, 2, 1)
	e.PlaceInSponsorTree(ctx, 3, 2)
	// Placement: 4 under 1 L, 5 under 4 R.
	e.PlaceInPlacementTree(ctx, 4, 1, "L")
	e.PlaceInPlacementTree(ctx, 5, 4, "R")

	var before int64
	h.DB.QueryRow(`SELECT COUNT(*) FROM mlm_closure`).Scan(&before)

	// Verify clean.
	if _, _, err := e.VerifyClosure(ctx); err != nil {
		t.Fatalf("pre-rebuild verify: %v", err)
	}
	// Rebuild: same count, same rows.
	n, err := e.RebuildClosure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != before {
		t.Fatalf("rebuild rows=%d on-write rows=%d (must match)", n, before)
	}
	var after int64
	h.DB.QueryRow(`SELECT COUNT(*) FROM mlm_closure`).Scan(&after)
	if after != before {
		t.Fatalf("after=%d before=%d", after, before)
	}
	// Outer-leg tags preserved.
	var leg string
	h.DB.QueryRow(`SELECT outer_leg FROM mlm_closure WHERE ancestor_id=1 AND descendant_id=5`).Scan(&leg)
	if leg != "L" {
		t.Fatalf("outer leg after rebuild=%q want L", leg)
	}
}

// RB2: drift detection + heal.
func TestRebuild_HealsDrift(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	e.PlaceInSponsorTree(ctx, 2, 1)
	e.PlaceInSponsorTree(ctx, 3, 2)

	// Simulate drift: delete a transitive row.
	h.DB.Exec(`DELETE FROM mlm_closure WHERE ancestor_id=1 AND descendant_id=3 AND outer_leg IS NULL`)

	_, _, err := e.VerifyClosure(ctx)
	if err == nil {
		t.Fatal("drift not detected")
	}
	if _, err := e.RebuildClosure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.VerifyClosure(ctx); err != nil {
		t.Fatalf("post-heal verify: %v", err)
	}
	// Levels correct after heal.
	var lvl int64
	h.DB.QueryRow(`SELECT level FROM mlm_closure WHERE ancestor_id=1 AND descendant_id=3`).Scan(&lvl)
	if lvl != 2 {
		t.Fatalf("level=%d want 2", lvl)
	}
}
