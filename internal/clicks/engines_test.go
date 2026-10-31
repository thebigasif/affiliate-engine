package clicks_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// Two batches for the same partner and day both land, on every engine.
// The second batch takes the upsert's UPDATE branch, where Postgres needs
// the table-qualified column.
func TestCommitter_SecondBatchSameDay_Engines(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	c := clicks.NewCommitter(ctx, h.DB, 100, 5*time.Millisecond)
	t.Cleanup(c.Close)
	for i := 0; i < 2; i++ {
		_, wg, err := c.Enqueue(1, 1, clicks.Click{IPHash: hash(byte(i))}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		wg.Wait()
	}
	var raw, agg, dc int64
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&raw)
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&agg)
	h.DB.QueryRow(`SELECT COALESCE(SUM(total_clicks),0) FROM dc_click_daily`).Scan(&dc)
	if raw != 2 || agg != 2 || dc != 2 {
		t.Fatalf("clicks=%d daily_agg=%d dc_click_daily=%d, want 2 each", raw, agg, dc)
	}
}
