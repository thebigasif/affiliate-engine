package mlm_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

// Dual-engine (BUILD-PLAN §3): the suite runs on file-backed SQLite by
// default and on Postgres when AE_TEST_ENGINES=postgres + admin DSN are
// set (CI does both). Never :memory: — concurrency semantics.
func newEnv(t *testing.T) (*db.Handles, *mlm.Engine) {
	t.Helper()
	h := dbtest.Open(t)
	for i := 1; i <= 5; i++ {
		if _, err := h.DB.Exec(
			`INSERT INTO affiliates (id, email) VALUES (?, ?)`, i, email(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Tier rates: L1 30%, L2 10% (the classic 2-tier split; basis points).
	h.DB.Exec(`INSERT INTO mlm_tier_rates (campaign_id, level, rate_bp) VALUES (1, 1, 3000)`)
	h.DB.Exec(`INSERT INTO mlm_tier_rates (campaign_id, level, rate_bp) VALUES (1, 2, 1000)`)
	return h, mlm.New(h.DB)
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

func email(i int) string { return string(rune('a'-1+i)) + "@t.dev" }

// Chain: 1 <- 2 <- 3 (3 sponsored by 2, 2 sponsored by 1).
func chain(t *testing.T, e *mlm.Engine) {
	t.Helper()
	ctx := context.Background()
	if err := e.PlaceInSponsorTree(ctx, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.PlaceInSponsorTree(ctx, 3, 2); err != nil {
		t.Fatal(err)
	}
}

// M1: sponsor tree closure: direct + transitive edges at right levels.
func TestMLM_SponsorClosure(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	var l12, l13 int
	h.DB.QueryRow(`SELECT level FROM mlm_closure WHERE ancestor_id=1 AND descendant_id=2 AND outer_leg IS NULL`).Scan(&l12)
	h.DB.QueryRow(`SELECT level FROM mlm_closure WHERE ancestor_id=1 AND descendant_id=3 AND outer_leg IS NULL`).Scan(&l13)
	if l12 != 1 || l13 != 2 {
		t.Fatalf("levels: 1->2=%d 1->3=%d (want 1, 2)", l12, l13)
	}
}

// M2: cycles rejected.
func TestMLM_CycleRejected(t *testing.T) {
	_, e := newEnv(t)
	chain(t, e)
	if err := e.PlaceInSponsorTree(context.Background(), 1, 3); err == nil {
		t.Fatal("cycle accepted: 1 under 3 while 3 is under 2 under 1")
	}
}

// M3: the 30/10/60 split — $100 commission to affiliate 3: L1 sponsor (2)
// gets 30, L2 sponsor (1) gets 10, earner keeps 60. Exact integers.
func TestMLM_TwoTierSplit(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	// Commission of 10000 cents to affiliate 3 in the window.
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('mj1','conversion', ?)`, nowRFC())
	h.DB.Exec(`INSERT INTO commissions (id, affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (50, 3, (SELECT id FROM journal_entries WHERE idem_key='mj1'), 10000, '{}', 'approved', ?)`,
		time.Now().UTC().Format(time.RFC3339))

	now := time.Now().UTC()
	lines, err := e.RunPeriod(ctx, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if lines != 2 {
		t.Fatalf("lines=%d want 2 (L1 + L2)", lines)
	}
	var periodID int64
	h.DB.QueryRow(`SELECT id FROM mlm_payout_periods LIMIT 1`).Scan(&periodID)

	tot2, _ := e.PeriodTotal(ctx, periodID, 2)
	tot1, _ := e.PeriodTotal(ctx, periodID, 1)
	if tot2["USD"] != 3000 || len(tot2) != 1 {
		t.Fatalf("L1 (affiliate 2) = %v want USD 3000", tot2)
	}
	if tot1["USD"] != 1000 || len(tot1) != 1 {
		t.Fatalf("L2 (affiliate 1) = %v want USD 1000", tot1)
	}
	// Earner keeps the rest (their own commission untouched: 10000 - splits).
	// Split total (4000) + earner 10000 = the amounts never exceed source.
	var splitSum int64
	h.DB.QueryRow(`SELECT SUM(commission_cents) FROM mlm_period_lines`).Scan(&splitSum)
	if splitSum != 4000 {
		t.Fatalf("split sum=%d want 4000", splitSum)
	}
}

// M4: re-running the same period never double-pays (unique key no-ops).
func TestMLM_PeriodIdempotent(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('mj2','conversion', ?)`, nowRFC())
	h.DB.Exec(`INSERT INTO commissions (id, affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (51, 3, (SELECT id FROM journal_entries WHERE idem_key='mj2'), 10000, '{}', 'approved', ?)`,
		time.Now().UTC().Format(time.RFC3339))
	now := time.Now().UTC()
	if _, err := e.RunPeriod(ctx, now.Add(-24*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Second run on the SAME window: refused (status paid).
	if _, err := e.RunPeriod(ctx, now.Add(-24*time.Hour), now.Add(time.Hour)); err == nil {
		t.Fatal("paid period re-ran")
	}
	var sum int64
	h.DB.QueryRow(`SELECT SUM(commission_cents) FROM mlm_period_lines`).Scan(&sum)
	if sum != 4000 {
		t.Fatalf("re-run changed totals: %d", sum)
	}
}

// M5: placement tree: binary legs tagged, occupancy enforced, separate from sponsor.
func TestMLM_PlacementTree(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	if err := e.PlaceInPlacementTree(ctx, 4, 1, "L"); err != nil {
		t.Fatal(err)
	}
	if err := e.PlaceInPlacementTree(ctx, 5, 4, "R"); err != nil {
		t.Fatal(err)
	}
	// Leg occupancy: 1's L leg taken by 4.
	if err := e.PlaceInPlacementTree(ctx, 5, 1, "L"); err == nil {
		t.Fatal("occupied leg accepted")
	}
	// Outer-leg tagging: 5 hangs under 1 through 4's L leg.
	var leg string
	h.DB.QueryRow(`SELECT outer_leg FROM mlm_closure WHERE ancestor_id=1 AND descendant_id=5`).Scan(&leg)
	if leg != "L" {
		t.Fatalf("outer leg=%q want L (inherited)", leg)
	}
	// Placement rows never appear in sponsor walks.
	var mixed int
	h.DB.QueryRow(`SELECT COUNT(*) FROM mlm_closure WHERE outer_leg IS NULL AND descendant_id IN (4,5)`).Scan(&mixed)
	if mixed != 0 {
		t.Fatalf("placement leaked into sponsor tree: %d", mixed)
	}
}

// M6: BV counters — personal + group up the sponsor chain.
func TestMLM_BVCounters(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	if err := e.RecordBV(ctx, 3, 20260915, "USD", 500); err != nil {
		t.Fatal(err)
	}
	var p3, g2, g1 int64
	h.DB.QueryRow(`SELECT bv_personal FROM mlm_volume_daily WHERE affiliate_id=3`).Scan(&p3)
	h.DB.QueryRow(`SELECT bv_group FROM mlm_volume_daily WHERE affiliate_id=2`).Scan(&g2)
	h.DB.QueryRow(`SELECT bv_group FROM mlm_volume_daily WHERE affiliate_id=1`).Scan(&g1)
	if p3 != 500 || g2 != 500 || g1 != 500 {
		t.Fatalf("bv: personal3=%d group2=%d group1=%d", p3, g2, g1)
	}
}
