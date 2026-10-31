package mlm_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

func mlm0(payout, cap int64) mlm.PairConfig {
	return mlm.PairConfig{PairPayoutCents: payout, MaxPairsPerDay: cap}
}

// PR1: pair = min(L, R); weaker leg governs; settle idempotent; the daily
// cap counts pairs already paid that day.
func TestPairing_Basics(t *testing.T) {
	_, e := newEnv(t)
	ctx := context.Background()
	const day = int64(20260915)
	cfg := mlm0(1000, 5) // $10/pair, max 5/day

	// Affiliate 1: L=3, R=7 => 3 pairs.
	e.RecordLegVolume(ctx, 1, "L", day, 3)
	e.RecordLegVolume(ctx, 1, "R", day, 7)
	pairs, payout, err := e.SettlePairs(ctx, 1, day, cfg)
	if err != nil || pairs != 3 || payout != 3000 {
		t.Fatalf("pairs=%d payout=%d err=%v (want 3, 3000)", pairs, payout, err)
	}
	// Re-settle: nothing new.
	pairs2, payout2, _ := e.SettlePairs(ctx, 1, day, cfg)
	if pairs2 != 0 || payout2 != 0 {
		t.Fatalf("re-settle paid again: %d/%d", pairs2, payout2)
	}
	// More L volume: L=6 => min(6,7)-3 = 3 new pairs, but only 5-3 = 2
	// remain under today's cap.
	e.RecordLegVolume(ctx, 1, "L", day, 3)
	pairs3, payout3, _ := e.SettlePairs(ctx, 1, day, cfg)
	if pairs3 != 2 || payout3 != 2000 {
		t.Fatalf("capped carry-forward: %d/%d want 2/2000", pairs3, payout3)
	}
}

// PR2: the daily cap bounds pairs paid for the DAY, not per settle call —
// repeated settles on one day must not each pay a fresh cap.
func TestPairing_DailyCap(t *testing.T) {
	_, e := newEnv(t)
	ctx := context.Background()
	const day = int64(20260916)
	cfg := mlm0(500, 2) // cap 2

	e.RecordLegVolume(ctx, 1, "L", day, 10)
	e.RecordLegVolume(ctx, 1, "R", day, 10)
	pairs, payout, _ := e.SettlePairs(ctx, 1, day, cfg)
	if pairs != 2 || payout != 1000 {
		t.Fatalf("cap not applied: %d/%d", pairs, payout)
	}
	for i := 0; i < 3; i++ {
		if p, c, err := e.SettlePairs(ctx, 1, day, cfg); err != nil || p != 0 || c != 0 {
			t.Fatalf("settle %d same day: pairs=%d payout=%d err=%v (cap already reached)", i, p, c, err)
		}
	}
	// A new day has a fresh cap.
	const next = int64(20260917)
	e.RecordLegVolume(ctx, 1, "L", next, 10)
	e.RecordLegVolume(ctx, 1, "R", next, 10)
	if p, _, _ := e.SettlePairs(ctx, 1, next, cfg); p != 2 {
		t.Fatalf("next day: %d want 2", p)
	}
}

// PR3: closure-driven upline legs — descendant volume attributes to the
// ancestor's correct outer leg.
func TestPairing_ClosureLegs(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	const day = int64(20260917)
	// Tree: 4 under 1 L; 5 under 4 R. So 5's volume hits 4's R leg and 1's L leg.
	e.PlaceInPlacementTree(ctx, 4, 1, "L")
	e.PlaceInPlacementTree(ctx, 5, 4, "R")

	// Volume from 5: 4 sees it on R (5 is 4's direct R child), 1 sees it on L
	// (the subtree hangs off 1's L).
	rows, err := h.DB.Query(`
		SELECT ancestor_id, outer_leg FROM mlm_closure WHERE descendant_id = 5`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	legs := map[string]string{}
	for rows.Next() {
		var a int64
		var leg string
		rows.Scan(&a, &leg)
		legs[string(rune('0'+a))] = leg
	}
	if legs["4"] != "R" || legs["1"] != "L" {
		t.Fatalf("closure legs: %+v", legs)
	}

	// Fold: 4 gets R volume, 1 gets L volume.
	e.RecordLegVolume(ctx, 4, legs["4"], day, 2)
	e.RecordLegVolume(ctx, 1, legs["1"], day, 2)
	// Complete 1's pair with an R.
	e.RecordLegVolume(ctx, 1, "R", day, 1)
	pairs, _, _ := e.SettlePairs(ctx, 1, day, mlm0(1000, 0))
	if pairs != 1 {
		t.Fatalf("upline pair: %d want 1", pairs)
	}
}
