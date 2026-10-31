// bench: self-contained performance measurement (FR-36 / BUILD-PLAN W5
// step 29). Unlike `make bench` (go test benchmarks, needs a toolchain)
// or cmd/loadtest (needs a running server), this runs from the release
// binary alone — an operator drops it on the target VPS and gets the two
// PRD-anchored numbers:
//
//	clicks/s sustained through the production committer (W1 gate >=1k/s)
//	dashboard query p95                     (NFR-3 gate <=10ms)
//
// It builds its own throwaway database (os.MkdirTemp) and never touches a
// real install. Exit 0 = all enforced gates green; exit 1 = a gate failed.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// PRD-anchored gates and measurement shapes. clickGateMinN mirrors the go
// benchmark's rule: at small N the pipeline is latency-bound by the 50ms
// flush tick, not throughput — enforcing clicks/s there measures the
// timer, not the machine.
const (
	clickGatePerSec = 1000 // BUILD-PLAN W1: >=1k clicks/s sustained
	clickGateMinN   = 5000 // below this, the clicks/s gate is advisory
	dashGateP95ms   = 10.0 // NFR-3: dashboard p95 <= 10ms
	dashAffiliates  = 10   // NFR-3 measurement shape: 10 affiliates...
	dashDays        = 30   // ...x 30 days of aggregates
	dashRowsPerDay  = 120  // clicks per (day, affiliate) row
	benchProducers  = 8    // producer cap (mirrors the go benchmark)
	benchBatchMax   = 500  // production committer config
	benchFlushEvery = 50 * time.Millisecond
)

// benchReport is the -json shape (CI-consumable).
type benchReport struct {
	Clicks             int     `json:"clicks"`
	ClicksPerSec       int64   `json:"clicks_per_sec"`
	ClickGateEnforced  bool    `json:"click_gate_enforced"`
	ClickGateOK        *bool   `json:"click_gate_ok,omitempty"`
	DashboardP50Ms     float64 `json:"dashboard_p50_ms"`
	DashboardP95Ms     float64 `json:"dashboard_p95_ms"`
	DashboardGateOK    bool    `json:"dashboard_gate_ok"`
	DriftOK            bool    `json:"drift_ok"`
	AllOK              bool    `json:"all_ok"`
	DashboardIteration int     `json:"dashboard_iterations"`
}

func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	nClicks := fs.Int("clicks", 20000, "clicks to push through the committer")
	dashIters := fs.Int("dashboard-iterations", 2000, "dashboard query iterations for p50/p95")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	gate := fs.Bool("gate", true, "enforce PRD gates (exit 1 on failure)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *nClicks < 1 || *dashIters < 1 {
		return fmt.Errorf("bench: -clicks and -dashboard-iterations must be >= 1")
	}

	tmpDir, err := os.MkdirTemp("", "ae-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := db.Open(ctx, "sqlite:"+filepath.Join(tmpDir, "bench.db"), db.Options{LedgerSyncFull: true})
	if err != nil {
		return err
	}
	defer h.DB.Close()
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		return err
	}
	for i := 1; i <= dashAffiliates; i++ {
		if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (?, ?)`,
			int64(i), fmt.Sprintf("bench%d@t.dev", i)); err != nil {
			return err
		}
	}

	rep := benchReport{Clicks: *nClicks, DashboardIteration: *dashIters}

	// --- 1) Click pipeline: the W1 gate measurement (also proves the
	// drift rule: aggregates must equal raw rows exactly). ---
	perSec, err := benchClickPipeline(ctx, h, *nClicks, cancel)
	if err != nil {
		return err
	}
	rep.ClicksPerSec = perSec
	rep.DriftOK = true
	rep.ClickGateEnforced = *gate && *nClicks >= clickGateMinN
	if rep.ClickGateEnforced {
		ok := perSec >= clickGatePerSec
		rep.ClickGateOK = &ok
	}

	// --- 2) Dashboard query p50/p95: the NFR-3 measurement. ---
	p50, p95, err := benchDashboard(ctx, h, *dashIters)
	if err != nil {
		return err
	}
	rep.DashboardP50Ms, rep.DashboardP95Ms = p50, p95
	rep.DashboardGateOK = p95 <= dashGateP95ms

	rep.AllOK = rep.DashboardGateOK && (!rep.ClickGateEnforced || *rep.ClickGateOK)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Printf("click pipeline : %6d clicks -> %d clicks/s", *nClicks, perSec)
		switch {
		case rep.ClickGateEnforced:
			fmt.Printf(" (gate >= %d/s: %s)", clickGatePerSec, pass(*rep.ClickGateOK))
		case !*gate:
			fmt.Print(" (gates disabled)")
		default:
			fmt.Printf(" (gate not enforced below %d clicks)", clickGateMinN)
		}
		fmt.Println()
		fmt.Printf("dashboard p50  : %8.3f ms\n", p50)
		fmt.Printf("dashboard p95  : %8.3f ms (gate <= %.0fms: %s)\n",
			p95, dashGateP95ms, pass(rep.DashboardGateOK))
		fmt.Printf("drift check    : aggregates == raw: %s\n", pass(rep.DriftOK))
		if rep.AllOK {
			fmt.Println("bench: ALL ENFORCED GATES PASSED")
		} else {
			fmt.Println("bench: GATE FAILURES DETECTED")
		}
	}

	if *gate && !rep.AllOK {
		os.Exit(1)
	}
	return nil
}

// benchClickPipeline pushes n clicks through the production committer
// (group-commit, batch <=500, 50ms flush — the serve-path configuration)
// and returns sustained clicks/s. Producers mirror redirect handlers; the
// timed region covers enqueue through durable commit of every click,
// then verifies the W1 drift rule (aggregates == raw == uniques ==
// attributions) before returning.
func benchClickPipeline(ctx context.Context, h *db.Handles, n int, cancel context.CancelFunc) (int64, error) {
	c := clicks.NewCommitter(ctx, h.DB, benchBatchMax, benchFlushEvery)
	defer c.Close()

	producers := runtime.GOMAXPROCS(0)
	if producers > benchProducers {
		producers = benchProducers
	}
	if producers < 1 {
		producers = 1
	}

	// Clicks march forward from 2026-09-01 (September only for any sane N);
	// the dashboard grid below seeds October so the two never collide.
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	start := time.Now()

	finals := make([]*sync.WaitGroup, producers)
	var done sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	done.Add(producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer done.Done()
			var wg *sync.WaitGroup
			for i := p; i < n; i += producers {
				cl := clicks.Click{
					IPHash:                 benchHash(i),
					UTMSource:              "bench",
					Placement:              fmt.Sprintf("pl_%d", i%16),
					Country:                "US",
					AttributionAffiliateID: int64(i%dashAffiliates + 1),
					AttributionCampaignID:  1,
					AttributionTTL:         30 * 24 * time.Hour,
				}
				var err error
				_, wg, err = c.Enqueue(int64(i%dashAffiliates+1), 1, cl, base.Add(time.Duration(i)*time.Second))
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					cancel() // unblock sibling producers
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
	elapsed := time.Since(start)
	if firstErr != nil {
		return 0, firstErr
	}

	// Drift rule (W1 exit test): byte-identical aggregates.
	var raw, agg, uniq, attr int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&raw); err != nil {
		return 0, err
	}
	if err := h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&agg); err != nil {
		return 0, err
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM uniq_daily`).Scan(&uniq); err != nil {
		return 0, err
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM attributions`).Scan(&attr); err != nil {
		return 0, err
	}
	if raw != n || agg != raw || uniq != raw || attr != raw {
		return 0, fmt.Errorf("bench: drift — raw=%d want %d, agg=%d, uniq=%d, attr=%d",
			raw, n, agg, uniq, attr)
	}

	if elapsed <= 0 {
		return 0, fmt.Errorf("bench: non-positive elapsed")
	}
	return int64(n) * int64(time.Second) / int64(elapsed), nil
}

// benchDashboard seeds the NFR-3 grid (10 affiliates x 30 October days,
// distinct from the pipeline's September clicks) and times the admin
// dashboard's aggregate query. Returns p50/p95 in milliseconds.
func benchDashboard(ctx context.Context, h *db.Handles, iterations int) (p50, p95 float64, err error) {
	for d := 1; d <= dashDays; d++ {
		day := fmt.Sprintf("202610%02d", d)
		for a := int64(1); a <= dashAffiliates; a++ {
			// Clicks in the currency-less row, sales in the USD row.
			if _, err := h.DB.Exec(`
				INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents)
				VALUES (?, ?, 1, '', ?, 0, 0), (?, ?, 1, 'USD', 0, 3, 990)`, day, a, dashRowsPerDay, day, a); err != nil {
				return 0, 0, err
			}
		}
	}

	lat := make([]float64, 0, iterations)
	for i := 0; i < iterations; i++ {
		s := time.Now()
		// Money per currency, as the dashboard reads it.
		var c64 int64
		rows, err := h.DB.QueryContext(ctx, `
			SELECT currency, COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0), COALESCE(SUM(commission_cents),0)
			FROM daily_agg GROUP BY currency`)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var cur string
			var clicks, conv, cents int64
			if err := rows.Scan(&cur, &clicks, &conv, &cents); err != nil {
				_ = rows.Close()
				return 0, 0, err
			}
			c64 += clicks
		}
		if err := rows.Close(); err != nil {
			return 0, 0, err
		}
		if c64 == 0 {
			return 0, 0, fmt.Errorf("bench: dashboard query returned no data")
		}
		lat = append(lat, float64(time.Since(s))/float64(time.Millisecond))
	}
	sort.Float64s(lat)
	pct := func(p float64) float64 {
		return lat[int(p*float64(len(lat)-1))]
	}
	return pct(0.50), pct(0.95), nil
}

// benchHash returns a deterministic, collision-free 16-byte identity hash
// for click index i (production hashes are computed at the edge, before
// Enqueue — no hashing cost belongs inside the timed region).
func benchHash(i int) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[0:], 0x62656e6368636c69) // "benchcli"
	binary.BigEndian.PutUint64(b[8:], uint64(uint(i)))    // G115-safe: loop index, non-negative
	return b
}
