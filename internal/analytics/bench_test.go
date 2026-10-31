package analytics_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// benchEnv builds a seeded analytics DB. Benchmarks seed once per invocation
// (outside the timed loop) and never mutate during timing, so b.N loops are
// pure read cost — the p95-dashboards NFR contract.
func benchEnv(b *testing.B) *db.Handles {
	b.Helper()
	ctx := context.Background()
	path := fmt.Sprintf("%s/bench.db", b.TempDir())
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		b.Fatal(err)
	}
	// Seed: 10 affiliates x 30 days x 3 campaigns of aggregates.
	for a := int64(1); a <= 10; a++ {
		if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (?, ?)`,
			a, fmt.Sprintf("b%d@x.dev", a)); err != nil {
			b.Fatal(err)
		}
		for d := 1; d <= 30; d++ {
			day := fmt.Sprintf("202608%02d", d)
			if _, err := h.DB.Exec(`
				INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents)
				VALUES (?, ?, 1, '', 120, 0, 0), (?, ?, 1, 'USD', 0, 3, 990)`, day, a, day, a); err != nil {
				b.Fatal(err)
			}
		}
	}
	return h
}

// BenchmarkDashboardQuery is the NFR-3 anchor: the admin dashboard's
// aggregate query shape over a 30-day, 10-affiliate program.
func BenchmarkDashboardQuery(b *testing.B) {
	h := benchEnv(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var clicks, conv, cents int64
		if err := h.DB.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0), COALESCE(SUM(commission_cents),0)
			FROM daily_agg`).Scan(&clicks, &conv, &cents); err != nil {
			b.Fatal(err)
		}
		if clicks != 10*30*120 {
			b.Fatalf("drift: clicks=%d", clicks)
		}
	}
}

// BenchmarkTokenStatsRollup: watermark rollup cost per invocation (the
// token-dimension analytics pipeline core).
func BenchmarkTokenStatsRollup(b *testing.B) {
	h := benchEnv(b)
	ts := analytics.NewTokenStats(h.DB)
	ctx := context.Background()
	// Seed clicks with the token dims the rollup aggregates.
	for d := 1; d <= 30; d++ {
		day := fmt.Sprintf("2026%02d%02d", 8, d)
		ts := fmt.Sprintf("2026-08-%02dT12:00:00Z", d)
		for a := int64(1); a <= 5; a++ {
			for _, src := range []string{"newsletter", "twitter", "blog"} {
				if _, err := h.DB.Exec(`
					INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id,
					                    utm_source, subid, placement)
					VALUES (?, ?, ?, ?, ?, 1, ?, ?, 'sidebar')`,
					fmt.Sprintf("bc-%d-%d-%s", d, a, src), ts, day, 202608, a, src, src); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ts.Rollup(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
