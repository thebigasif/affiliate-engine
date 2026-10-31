package gamify

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

func newEnvInternal(t *testing.T) *Engine {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'x@t.dev')`)
	e := New(h.DB)
	if err := e.SeedBadges(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

// G9: day-math helpers never cross month/year boundaries by subtraction.
func TestPrevDayBoundaries(t *testing.T) {
	cases := map[int64]int64{
		20260301: 20260228, // non-leap February
		20240301: 20240229, // leap February
		20260101: 20251231, // year wrap
		20260916: 20260915, // plain day
		20260102: 20260101,
		20260501: 20260430, // 30-day month
		20260701: 20260630,
		20261231: 20261230,
	}
	for in, want := range cases {
		if got := prevDay(in); got != want {
			t.Fatalf("prevDay(%d)=%d want %d", in, got, want)
		}
	}
}

// G11: the streak walk crosses month and year boundaries correctly with a
// fixed anchor date (deterministic — no dependence on the wall clock).
func TestActiveStreakMonthBoundary(t *testing.T) {
	e := newEnvInternal(t)
	ctx := context.Background()
	// 35 consecutive days ending 2026-03-05: spans Mar->Feb (non-leap 28)
	// and Feb->Jan. Anchor "now" at 2026-03-05 23:00 UTC.
	end := time.Date(2026, 3, 5, 23, 0, 0, 0, time.UTC)
	for i := 0; i < 35; i++ {
		d := end.AddDate(0, 0, -i).Format("20060102")
		if _, err := e.db.ExecContext(ctx,
			`INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks) VALUES (`+d+`, 1, 1, 5)`); err != nil {
			t.Fatal(err)
		}
	}
	if s := e.activeStreakDays(ctx, 1, end); s != 35 {
		t.Fatalf("streak=%d want 35 (month boundary broken)", s)
	}
	// Remove 2026-02-28: streak collapses to 5 (Mar 1..5).
	if _, err := e.db.ExecContext(ctx,
		`DELETE FROM daily_agg WHERE day = 20260228 AND affiliate_id = 1`); err != nil {
		t.Fatal(err)
	}
	if s := e.activeStreakDays(ctx, 1, end); s != 5 {
		t.Fatalf("streak=%d want 5 after gap", s)
	}
}

// G12: year-boundary streak (Dec 2026 -> Jan 2027).
func TestActiveStreakYearBoundary(t *testing.T) {
	e := newEnvInternal(t)
	ctx := context.Background()
	end := time.Date(2027, 1, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		d := end.AddDate(0, 0, -i).Format("20060102")
		if _, err := e.db.ExecContext(ctx,
			`INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks) VALUES (`+d+`, 1, 1, 5)`); err != nil {
			t.Fatal(err)
		}
	}
	if s := e.activeStreakDays(ctx, 1, end); s != 10 {
		t.Fatalf("streak=%d want 10 (year boundary broken)", s)
	}
}

// G13: empty history -> streak 0, no error.
func TestActiveStreakEmpty(t *testing.T) {
	e := newEnvInternal(t)
	if s := e.activeStreakDays(context.Background(), 1, time.Now().UTC()); s != 0 {
		t.Fatalf("streak=%d want 0", s)
	}
}
