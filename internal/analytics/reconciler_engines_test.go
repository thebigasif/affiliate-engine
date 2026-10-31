package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// The second tick of a day adds to an existing daily_agg row (the upsert's
// DO UPDATE path). On Postgres an unqualified "clicks" in that SET was
// ambiguous with the clicks table and failed every tick.
func TestReconciler_UpsertOnEveryEngine(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("20060102")
	add := func(from, n int) {
		for i := from; i < from+n; i++ {
			if _, err := h.DB.Exec(`
				INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, bot_class, exclude_stats)
				VALUES (?, ?, ?, ?, 1, 1, 0, 0)`,
				fmt.Sprintf("c%03d", i), today[:4]+"-"+today[4:6]+"-"+today[6:]+"T12:00:00Z", today, today[:6]); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := analytics.NewReconciler(h.DB)
	add(0, 4)
	if _, err := r.Tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	add(4, 3)
	if _, err := r.Tick(ctx); err != nil {
		t.Fatalf("second tick (upsert): %v", err)
	}
	var clicks int64
	if err := h.DB.QueryRow(`SELECT clicks FROM daily_agg WHERE day = ? AND affiliate_id = 1 AND campaign_id = 1`, today).Scan(&clicks); err != nil {
		t.Fatal(err)
	}
	if clicks != 7 {
		t.Fatalf("daily_agg clicks = %d, want 7", clicks)
	}
}
