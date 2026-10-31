package analytics_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// NFR-7 pin: clicks older than the window are pruned; aggregates kept.
func TestRetention_SweepsOldClicks(t *testing.T) {
	h := dbtest.Open(t)
	r := analytics.NewRetention(h.DB, 30*24*time.Hour)

	old := time.Now().UTC().AddDate(0, 0, -60)
	fresh := time.Now().UTC().AddDate(0, 0, -1)
	od := int64(old.Year())*10000 + int64(old.Month())*100 + int64(old.Day())
	fd := int64(fresh.Year())*10000 + int64(fresh.Month())*100 + int64(fresh.Day())

	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'r@t.dev')`)
	for i, d := range []int64{od, fd} {
		h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
			VALUES (?, ?, ?, ?, 1, 1)`,
			[]string{"old1", "new1"}[i], time.Now().UTC().Format(time.RFC3339Nano), d, d/100)
	}
	// Aggregate rows at both ages must SURVIVE.
	h.DB.Exec(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks) VALUES (?, 1, 1, 5)`, od)

	n, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned=%d want 1", n)
	}
	var clicks int64
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&clicks)
	if clicks != 1 {
		t.Fatalf("clicks left=%d want 1 (fresh)", clicks)
	}
	var agg int64
	h.DB.QueryRow(`SELECT COUNT(*) FROM daily_agg`).Scan(&agg)
	if agg != 1 {
		t.Fatalf("aggregates pruned=%d — retention must keep them", 1-agg)
	}
	_ = db.Handles{}
}
