package gamify_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/gamify"
)

func newEnv(t *testing.T) (*db.Handles, *gamify.Engine) {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'x@t.dev'), (2, 'y@t.dev')`)
	e := gamify.New(h.DB)
	if err := e.SeedBadges(ctx); err != nil {
		t.Fatal(err)
	}
	return h, e
}

func commission(t *testing.T, h *db.Handles, id, aff, cents int64) {
	t.Helper()
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('g` + fmtI(id) + `','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (id, affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (?, ?, (SELECT id FROM journal_entries WHERE idem_key='g`+fmtI(id)+`'), ?, '{}', 'approved')`,
		id, aff, cents)
}

func fmtI(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// G1: first commission grants badge + XP; replays are no-ops.
func TestGamify_FirstCommission(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	commission(t, h, 1, 1, 500)

	if err := e.OnCommission(ctx, 1, 1, 500); err != nil {
		t.Fatal(err)
	}
	// Replay: nothing double-granted.
	if err := e.OnCommission(ctx, 1, 1, 500); err != nil {
		t.Fatal(err)
	}
	p, _ := e.Profile(ctx, 1)
	if p["xp"].(int64) != 600 { // 500 first + 100 conversion
		t.Fatalf("xp=%v want 600 (no double)", p["xp"])
	}
	badges := p["badges"].([]map[string]any)
	if len(badges) != 1 || badges[0]["code"] != "first_commission" {
		t.Fatalf("badges=%v", badges)
	}
}

// G2: milestones fire at thresholds exactly once.
func TestGamify_Milestones(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	// Commission of 1200 cents crosses earner_1k.
	commission(t, h, 10, 1, 1200)
	if err := e.OnCommission(ctx, 1, 10, 1200); err != nil {
		t.Fatal(err)
	}
	p, _ := e.Profile(ctx, 1)
	// 500 (first) + 100 (conversion) + 1000 (milestone 1k) = 1600
	if p["xp"].(int64) != 1600 {
		t.Fatalf("xp=%v want 1600", p["xp"])
	}
	found := map[string]bool{}
	for _, b := range p["badges"].([]map[string]any) {
		found[b["code"].(string)] = true
	}
	if !found["earner_1k"] {
		t.Fatalf("earner_1k missing: %v", found)
	}
	// Second commission pushing lifetime further: no milestone re-fire.
	commission(t, h, 11, 1, 500)
	e.OnCommission(ctx, 1, 11, 500)
	p2, _ := e.Profile(ctx, 1)
	if p2["xp"].(int64) != 1700 { // +100 conversion only
		t.Fatalf("xp=%v want 1700", p2["xp"])
	}
}

// G3: level curve + progression reporting.
func TestGamify_Levels(t *testing.T) {
	_, e := newEnv(t)
	ctx := context.Background()
	cases := map[int64]int64{0: 1, 99: 1, 400: 2, 2499: 4, 2500: 5, 9999: 9, 10000: 10, 999999: 99}
	for xp, want := range cases {
		if got := gamify.LevelForXP(xp); got != want {
			t.Fatalf("LevelForXP(%d)=%d want %d", xp, got, want)
		}
	}
	// Recompute detects level-ups.
	h := &db.Handles{}
	_ = h
	if l, up, err := e.RecomputeLevel(ctx, 1); err != nil || l != 1 || up {
		t.Fatalf("empty profile level: %d up=%v err=%v", l, up, err)
	}
}

// G4: leaderboard ranks by XP.
func TestGamify_Leaderboard(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	commission(t, h, 20, 1, 500)
	commission(t, h, 21, 2, 1500)
	e.OnCommission(ctx, 1, 20, 500)
	e.OnCommission(ctx, 2, 21, 1500) // 2 gets milestone too
	lb, err := e.Leaderboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 2 {
		t.Fatalf("lb=%v", lb)
	}
	if lb[0]["affiliate_id"].(int64) != 2 {
		t.Fatalf("rank1=%v want affiliate 2", lb[0])
	}
}
