package analytics_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// FR-26 dual-engine contract: the parquet export runs on SQLite AND
// Postgres (dbtest.Open honors AE_TEST_ENGINES) and roundtrips exactly —
// every click row and its bytes (hashes included) survive, day filters
// bound correctly, and row counts match the SQL side.

// seedExportEnv seeds clicks + aggregates on a fresh dual-engine handle.
func seededClicksDB(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'x@t.dev'), (2, 'y@t.dev')`); err != nil {
		t.Fatal(err)
	}
	// 3 clicks on 20260901 (one known-bot, one cookie-method NULL hashes),
	// 2 clicks on 20260902.
	dayN := map[string]int64{"20260901": 20260901, "20260902": 20260902}
	rows := []struct {
		id, day string
		aff     int64
		ipHash  []byte
		bot     int
		excl    int
	}{
		{"c1", "20260901", 1, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, 0, 0},
		{"c2", "20260901", 1, []byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, 0, 0},
		{"c3", "20260901", 2, nil, 2, 1}, // known bot, cookie-method
		{"c4", "20260902", 2, []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}, 0, 0},
		{"c5", "20260902", 1, []byte{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8}, 0, 0},
	}
	for _, r := range rows {
		if _, err := h.DB.Exec(`
			INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id,
				ip_hash, utm_source, placement, country, bot_class, exclude_stats)
			VALUES (?, ?, ?, ?, ?, 1, ?, 'nl', 'sidebar', 'US', ?, ?)`,
			r.id, r.day+"T12:00:00Z", dayN[r.day], dayN[r.day]/100, r.aff, r.ipHash, r.bot, r.excl); err != nil {
			t.Fatal(err)
		}
	}
	// Clicks in currency-less rows; money one row per currency.
	for _, a := range []struct {
		day       string
		aff       int64
		cur       string
		clicks    int64
		conv, amt int64
	}{
		{"20260901", 1, "", 120, 0, 0},
		{"20260901", 1, "USD", 0, 3, 990},
		{"20260902", 2, "", 120, 0, 0},
		{"20260902", 2, "JPY", 0, 1, 5000},
	} {
		if _, err := h.DB.Exec(`
			INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents)
			VALUES (?, ?, 1, ?, ?, ?, ?)`, dayN[a.day], a.aff, a.cur, a.clicks, a.conv, a.amt); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// Roundtrip: exported clicks read back identical (values + NULL hashes +
// bot flags); export ordered by (day, id).
func TestParquet_Roundtrip(t *testing.T) {
	h := seededClicksDB(t)
	dir := t.TempDir()
	nClicks, nAggs, err := analytics.ExportParquet(context.Background(), h.DB, dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if nClicks != 5 || nAggs != 4 {
		t.Fatalf("exported clicks=%d aggs=%d want 5/4", nClicks, nAggs)
	}

	back, err := analytics.ReadParquetClicks(filepath.Join(dir, "clicks.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 5 {
		t.Fatalf("read back %d clicks want 5", len(back))
	}
	byID := map[string]analytics.ClickRow{}
	for _, r := range back {
		byID[r.ID] = r
	}
	c3 := byID["c3"]
	if c3.IPHash != nil {
		t.Fatalf("c3 ip_hash: want NULL, got %d bytes", len(c3.IPHash))
	}
	if c3.BotClass != 2 || c3.ExcludeStats != 1 {
		t.Fatalf("c3 bot_class=%d exclude=%d want 2/1", c3.BotClass, c3.ExcludeStats)
	}
	c1 := byID["c1"]
	if len(c1.IPHash) != 16 || c1.IPHash[0] != 1 || c1.Country != "US" || c1.Placement != "sidebar" {
		t.Fatalf("c1 roundtrip mismatch: %+v", c1)
	}
	if back[0].ID != "c1" || back[len(back)-1].ID != "c5" {
		t.Fatalf("export not ordered by (day,id): first=%s last=%s", back[0].ID, back[len(back)-1].ID)
	}
}

// Day bounds: fromDay/toDay filter the clicks file only.
func TestParquet_DayBounds(t *testing.T) {
	h := seededClicksDB(t)
	ctx := context.Background()
	dir := t.TempDir()

	n, _, err := analytics.ExportParquet(ctx, h.DB, dir, 20260902, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("fromDay filter: %d clicks want 2", n)
	}
	back, err := analytics.ReadParquetClicks(filepath.Join(dir, "clicks.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range back {
		if r.Day != 20260902 {
			t.Fatalf("day bound leaked: %+v", r)
		}
	}

	n, _, err = analytics.ExportParquet(ctx, h.DB, filepath.Join(dir, "d2"), 0, 20260901)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("toDay filter: %d clicks want 3", n)
	}
}

// Empty database exports valid (empty) parquet files — not an error.
func TestParquet_Empty(t *testing.T) {
	h := dbtest.Open(t)
	nClicks, nAggs, err := analytics.ExportParquet(context.Background(), h.DB, t.TempDir(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if nClicks != 0 || nAggs != 0 {
		t.Fatalf("empty db exported %d/%d want 0/0", nClicks, nAggs)
	}
}

// Aggregates file: total clicks in parquet must equal SQL SUM(clicks) —
// the number a DuckDB sidecar reports must match the product dashboard.
func TestParquet_AggTotals(t *testing.T) {
	h := seededClicksDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	if _, nAggs, err := analytics.ExportParquet(ctx, h.DB, dir, 0, 0); err != nil {
		t.Fatal(err)
	} else if nAggs != 4 {
		t.Fatalf("aggs=%d want 4", nAggs)
	}

	var sqlTotal int64
	if err := h.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&sqlTotal); err != nil {
		t.Fatal(err)
	}
	aggRows, err := analytics.ReadParquetAggs(filepath.Join(dir, "daily_agg.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	var pqTotal int64
	for _, r := range aggRows {
		pqTotal += r.Clicks
	}
	if pqTotal != sqlTotal || pqTotal != 240 {
		t.Fatalf("parquet total=%d sql total=%d (want 240)", pqTotal, sqlTotal)
	}
}

// Money in the aggregates file carries its currency: a DuckDB query sums
// commission_cents per currency, never across.
func TestParquet_AggCurrency(t *testing.T) {
	h := seededClicksDB(t)
	dir := t.TempDir()
	if _, _, err := analytics.ExportParquet(context.Background(), h.DB, dir, 0, 0); err != nil {
		t.Fatal(err)
	}
	aggRows, err := analytics.ReadParquetAggs(filepath.Join(dir, "daily_agg.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range aggRows {
		if r.CommissionCents != 0 || r.Conversions != 0 {
			got[r.Currency] += r.CommissionCents
		} else if r.Currency != "" {
			t.Fatalf("click row with a currency: %+v", r)
		}
	}
	if len(got) != 2 || got["USD"] != 990 || got["JPY"] != 5000 {
		t.Fatalf("parquet money per currency = %v, want USD 990 and JPY 5000", got)
	}
}
