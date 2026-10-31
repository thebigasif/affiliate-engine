package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/db"
)

func newDB(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1,'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	return h
}

func insertClick(t *testing.T, h *db.Handles, id, day string, bot int) {
	t.Helper()
	if _, err := h.DB.Exec(`
		INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, bot_class, exclude_stats)
		VALUES (?, ?, ?, ?, 1, 1, ?, ?)`,
		id, day+"T12:00:00Z", day, day[:6], bot, botExcluded(bot)); err != nil {
		t.Fatal(err)
	}
}

func botExcluded(b int) int {
	if b == 2 {
		return 1
	}
	return 0
}

// RC1: incremental rollup from watermark — two ticks aggregate exactly once.
func TestReconciler_Incremental(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("20060102")
	for i := 0; i < 10; i++ {
		insertClick(t, h, fmt.Sprintf("c%03d", i), today, 0)
	}

	r := analytics.NewReconciler(h.DB)
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var clicks int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&clicks)
	if clicks != 10 {
		t.Fatalf("first tick: clicks=%d want 10", clicks)
	}
	// Second tick: watermark advanced, nothing double-counted.
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&clicks)
	if clicks != 10 {
		t.Fatalf("second tick double-counted: clicks=%d", clicks)
	}
	// New clicks after watermark: only the delta rolls up.
	for i := 10; i < 15; i++ {
		insertClick(t, h, fmt.Sprintf("c%03d", i), today, 0)
	}
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&clicks)
	if clicks != 15 {
		t.Fatalf("delta tick: clicks=%d want 15", clicks)
	}
}

// RC2: drift heal — a tampered aggregate for yesterday is repaired.
func TestReconciler_DriftHeal(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("20060102")
	for i := 0; i < 20; i++ {
		insertClick(t, h, fmt.Sprintf("d%03d", i), yesterday, 0)
	}
	r := analytics.NewReconciler(h.DB)
	if _, err := r.Tick(ctx); err != nil { // establishes baseline
		t.Fatal(err)
	}
	// Simulate drift: on-write path lost 5 clicks.
	if _, err := h.DB.Exec(`UPDATE daily_agg SET clicks = clicks - 5`); err != nil {
		t.Fatal(err)
	}
	var clicks int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE day=?`, yesterday).Scan(&clicks)
	if clicks != 15 {
		t.Fatalf("setup: clicks=%d want 15 (drifted)", clicks)
	}
	// Reconciler must detect and heal to 20.
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE day=?`, yesterday).Scan(&clicks)
	if clicks != 20 {
		t.Fatalf("heal failed: clicks=%d want 20", clicks)
	}
}

// RC2b: the heal puts clicks in the day's currency-less click row, never
// in a money row (a sale's row is per currency; clicks have none).
func TestReconciler_DriftHealSkipsMoneyRows(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("20060102")
	for i := 0; i < 4; i++ {
		insertClick(t, h, fmt.Sprintf("m%03d", i), yesterday, 0)
	}
	// The sale was aggregated; the clicks were lost.
	if _, err := h.DB.Exec(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, conversions, commission_cents)
		VALUES (?, 1, 1, 'USD', 1, 500)`, yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := analytics.NewReconciler(h.DB).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var blank, usd int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE day=? AND currency=''`, yesterday).Scan(&blank)
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE day=? AND currency='USD'`, yesterday).Scan(&usd)
	if blank != 4 || usd != 0 {
		t.Fatalf("healed clicks: click row=%d USD row=%d, want 4 and 0", blank, usd)
	}
}

// RC3: bots never counted in aggregates.
func TestReconciler_BotExclusion(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("20060102")
	insertClick(t, h, "human1", today, 0)
	insertClick(t, h, "bot1", today, 2)
	r := analytics.NewReconciler(h.DB)
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var clicks int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&clicks)
	if clicks != 1 {
		t.Fatalf("clicks=%d want 1 (bot excluded)", clicks)
	}
}

// RC4: dashboard reads from aggregates.
func TestReconciler_Dashboard(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("20060102")
	for i := 0; i < 7; i++ {
		insertClick(t, h, fmt.Sprintf("db%d", i), today, 0)
	}
	r := analytics.NewReconciler(h.DB)
	if _, err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	agg, err := r.Dashboard(ctx, today, today)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Clicks != 7 {
		t.Fatalf("dashboard clicks=%d want 7", agg.Clicks)
	}
	// Money is per currency: the yen row is not added to the cents.
	for _, q := range []string{
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, conversions, commission_cents) VALUES (?, 1, 1, 'USD', 1, 500)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, conversions, commission_cents) VALUES (?, 1, 1, 'JPY', 2, 9000)`,
	} {
		if _, err := h.DB.Exec(q, today); err != nil {
			t.Fatal(err)
		}
	}
	agg, err = r.Dashboard(ctx, today, today)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Clicks != 7 || agg.Conversions != 3 || len(agg.Commission) != 2 ||
		agg.Commission["USD"] != 500 || agg.Commission["JPY"] != 9000 {
		t.Fatalf("dashboard = %+v", agg)
	}
}
