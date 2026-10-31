package clicks_test

import (
	"context"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
)

func newDB(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	return h
}

func hash(n byte) []byte {
	b := make([]byte, 16)
	rand.Read(b)
	b[0] = n
	return b
}

// K1: single click flows through and lands in clicks + uniq_daily + daily_agg.
func TestCommitter_SingleClick(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	c := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(c.Close)

	_, wg, err := c.Enqueue(1, 1, clicks.Click{IPHash: hash(1), Placement: "pl_1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	var nClicks, nAgg, nUniq int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&nClicks)
	h.DB.QueryRow(`SELECT clicks FROM daily_agg`).Scan(&nAgg)
	h.DB.QueryRow(`SELECT COUNT(*) FROM uniq_daily`).Scan(&nUniq)
	if nClicks != 1 || nAgg != 1 || nUniq != 1 {
		t.Fatalf("clicks=%d agg=%d uniq=%d", nClicks, nAgg, nUniq)
	}
}

// K2: duplicate identity hash same day counts click but not unique.
func TestCommitter_UniqueDedup(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	c := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(c.Close)

	ip := hash(2)
	for i := 0; i < 5; i++ {
		_, wg, _ := c.Enqueue(1, 1, clicks.Click{IPHash: ip}, time.Now())
		wg.Wait()
	}
	var nClicks, nUniq int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&nClicks)
	h.DB.QueryRow(`SELECT COUNT(*) FROM uniq_daily`).Scan(&nUniq)
	if nClicks != 5 {
		t.Fatalf("clicks=%d want 5", nClicks)
	}
	if nUniq != 1 {
		t.Fatalf("uniques=%d want 1 (same identity, same day)", nUniq)
	}
}

// K3: known bots stored but excluded from aggregates.
func TestCommitter_BotExclusion(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	c := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(c.Close)

	_, wg, _ := c.Enqueue(1, 1, clicks.Click{IPHash: hash(3), BotClass: 2}, time.Now())
	wg.Wait()

	var nClicks, nAgg, nUniq int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&nClicks)
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&nAgg)
	h.DB.QueryRow(`SELECT COUNT(*) FROM uniq_daily`).Scan(&nUniq)
	if nClicks != 1 {
		t.Fatalf("raw click must be stored (forensics), got %d", nClicks)
	}
	if nAgg != 0 || nUniq != 0 {
		t.Fatalf("bot must be excluded from stats: agg=%d uniq=%d", nAgg, nUniq)
	}
}

// K4: concurrent producers, one committer — all clicks land, aggregates exact.
func TestCommitter_ConcurrentThroughput(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	c := clicks.NewCommitter(ctx, h.DB, 500, 50*time.Millisecond)
	t.Cleanup(c.Close)

	const producers, perProducer = 8, 100
	var wg sync.WaitGroup
	start := time.Now()
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				_, w, err := c.Enqueue(1, 1, clicks.Click{IPHash: hash(byte(p))}, time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				w.Wait()
			}
		}(p)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var nClicks int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&nClicks)
	if nClicks != producers*perProducer {
		t.Fatalf("clicks=%d want %d", nClicks, producers*perProducer)
	}
	var nAgg int
	h.DB.QueryRow(`SELECT clicks FROM daily_agg`).Scan(&nAgg)
	if nAgg != producers*perProducer {
		t.Fatalf("agg clicks=%d want %d", nAgg, producers*perProducer)
	}
	t.Logf("800 clicks in %v (%.0f/s)", elapsed, float64(producers*perProducer)/elapsed.Seconds())
}

// K5: watermark reconciler equivalence — daily_agg == raw count (non-bot).
func TestCommitter_AggregatesMatchRaw(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	c := clicks.NewCommitter(ctx, h.DB, 200, 20*time.Millisecond)
	t.Cleanup(c.Close)

	for i := 0; i < 300; i++ {
		bot := 0
		if i%7 == 0 {
			bot = 2
		}
		_, wg, _ := c.Enqueue(1, 1, clicks.Click{IPHash: hash(byte(i % 50)), BotClass: bot}, time.Now())
		wg.Wait()
	}
	var raw, agg int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks WHERE exclude_stats = 0`).Scan(&raw)
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&agg)
	if raw != agg {
		t.Fatalf("raw=%d agg=%d — reconciler contract broken", raw, agg)
	}
}
