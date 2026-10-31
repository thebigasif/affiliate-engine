package clicks_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// benchDB opens a file-backed SQLite database with the full migration chain.
// File-backed is mandatory (never :memory:) so commit semantics match
// production and the OS page cache is exercised.
func benchDB(b *testing.B) *db.Handles {
	b.Helper()
	ctx := context.Background()
	path := filepath.Join(b.TempDir(), "bench.db")
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{LedgerSyncFull: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (?, ?)`,
			int64(i), fmt.Sprintf("bench%d@t.dev", i)); err != nil {
			b.Fatal(err)
		}
	}
	return h
}

// benchHash returns a deterministic, collision-free 16-byte identity hash for
// click index i (production hashes are computed at the edge, before Enqueue —
// so no hashing cost belongs inside the timed region).
func benchHash(i int) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[0:], 0x62656e6368636c69) // "benchcli"
	binary.BigEndian.PutUint64(b[8:], uint64(i))
	return b
}

// BenchmarkClickPipeline is the W1 gate measurement: sustained end-to-end
// throughput of the production click path. Producers play redirect handlers
// (enqueue, backpressure included); the single committer goroutine group-
// commits batches of ≤500 exactly as in production, attribution rows in the
// same transaction (redirect-minted token shape). The timed region covers
// enqueue through visible commit of every click, then verifies aggregates
// against raw rows — the drift rule from the W1 exit test.
//
// Gate: ≥1k clicks/s sustained on dev hardware (BUILD-PLAN W1; measured
// ceiling 51k/s). The gate is only enforced at b.N ≥ gateMinN: at tiny N a
// single click is latency-bound by the flush tick (50ms), not throughput —
// the framework's N=1 warmup would otherwise fail 1/0.05s = 20/s.
func BenchmarkClickPipeline(b *testing.B) {
	const gateMinN = 1000
	h := benchDB(b)
	ctx := context.Background()
	// Production defaults: batch ≤500, flush every 50ms.
	c := clicks.NewCommitter(ctx, h.DB, 500, 50*time.Millisecond)
	// Producers deliberately outrun the writer here: measure throughput
	// under full backpressure instead of load-shedding (production sheds
	// after FullWait so a flood cannot stall redirects).
	c.FullWait = -1
	defer c.Close()

	producers := runtime.GOMAXPROCS(0)
	if producers > 8 {
		producers = 8
	}
	if producers < 1 {
		producers = 1
	}

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()

	// Each producer keeps only its LAST item's WaitGroup: the committer is a
	// single FIFO goroutine, so when the last enqueued item is committed every
	// earlier one is durable too.
	finals := make([]*sync.WaitGroup, producers)
	var done sync.WaitGroup
	done.Add(producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer done.Done()
			var wg *sync.WaitGroup
			for i := p; i < b.N; i += producers {
				cl := clicks.Click{
					IPHash:                 benchHash(i),
					UTMSource:              "bench",
					Placement:              fmt.Sprintf("pl_%d", i%16),
					Country:                "US",
					AttributionAffiliateID: int64(i%8 + 1),
					AttributionCampaignID:  1,
					AttributionTTL:         30 * 24 * time.Hour,
				}
				var err error
				_, wg, err = c.Enqueue(int64(i%8+1), 1, cl, base.Add(time.Duration(i)*time.Second))
				if err != nil {
					b.Error(err) // safe from goroutines (Fatal is not)
					return
				}
			}
			finals[p] = wg
		}(p)
	}
	done.Wait()
	for _, wg := range finals {
		if wg != nil {
			wg.Wait()
		}
	}
	b.StopTimer()

	// --- W1 drift rule: aggregates must equal raw (all clicks human-class). ---
	var raw, agg, uniq, attr int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&raw); err != nil {
		b.Fatal(err)
	}
	if err := h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&agg); err != nil {
		b.Fatal(err)
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM uniq_daily`).Scan(&uniq); err != nil {
		b.Fatal(err)
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM attributions`).Scan(&attr); err != nil {
		b.Fatal(err)
	}
	if raw != b.N {
		b.Fatalf("raw clicks=%d want %d (enqueue loss)", raw, b.N)
	}
	if agg != raw {
		b.Fatalf("aggregate drift: daily_agg=%d raw=%d", agg, raw)
	}
	if uniq != raw {
		b.Fatalf("uniq drift: uniq_daily=%d raw=%d (hashes not unique per click)", uniq, raw)
	}
	if attr != raw {
		b.Fatalf("attribution drift: attributions=%d raw=%d", attr, raw)
	}

	// --- W1 gate: ≥1k clicks/s sustained (only meaningful at scale). ---
	if b.N >= gateMinN {
		if el := b.Elapsed(); el > 0 {
			perSec := int64(b.N) * int64(time.Second) / int64(el)
			b.ReportMetric(float64(perSec), "clicks/s")
			if perSec < 1000 {
				b.Fatalf("throughput gate failed: %d clicks/s < 1000 (W1 gate)", perSec)
			}
		}
	}
}
