package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// readAgg reads an aggregate table as "day/affiliate/campaign/currency"
// -> {clicks, conversions, commission_cents} (token rows summed over
// their dimensions).
func readAgg(t *testing.T, db *sql.DB, table string) map[string][3]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT day, affiliate_id, campaign_id, currency,
		SUM(clicks), SUM(conversions), SUM(commission_cents) FROM ` + table + `
		GROUP BY day, affiliate_id, campaign_id, currency`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][3]int64{}
	for rows.Next() {
		var day, aff, camp, c, n, v int64
		var cur string
		if err := rows.Scan(&day, &aff, &camp, &cur, &c, &n, &v); err != nil {
			t.Fatal(err)
		}
		out[fmt.Sprintf("%d/%d/%d/%s", day, aff, camp, cur)] = [3]int64{c, n, v}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameAgg(a, b map[string][3]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// The aggregates of a database written before they had a currency are
// rebuilt per currency on upgrade. Clicks stay in currency-less rows. A
// partner whose sales were all in one currency keeps its rows (and days)
// as they were, tagged with that currency. A partner with sales in several
// currencies has its money rebuilt from its commission rows, one row per
// day and currency. Money with no commission behind it is the program
// currency's. Re-running changes nothing.
func TestAggregatesPerCurrencyMigration(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	for _, s := range []string{
		// The old shapes: no currency in the key.
		`DROP TABLE daily_agg`,
		`CREATE TABLE daily_agg (day INTEGER NOT NULL, affiliate_id BIGINT NOT NULL, campaign_id BIGINT NOT NULL,
			clicks BIGINT NOT NULL DEFAULT 0, conversions BIGINT NOT NULL DEFAULT 0,
			commission_cents BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (day, affiliate_id, campaign_id))`,
		`DROP TABLE token_stats_daily`,
		`CREATE TABLE token_stats_daily (day INTEGER NOT NULL, affiliate_id BIGINT NOT NULL, campaign_id BIGINT NOT NULL,
			dim TEXT NOT NULL, value TEXT NOT NULL, clicks INTEGER NOT NULL DEFAULT 0,
			conversions INTEGER NOT NULL DEFAULT 0, commission_cents BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (day, affiliate_id, campaign_id, dim, value))`,
		`UPDATE program_settings SET currency = 'EUR' WHERE id = 1`,
		`INSERT INTO affiliates (id, email) VALUES (1, 'multi@t.dev'), (2, 'usd@t.dev'), (3, 'clicks@t.dev'), (4, 'orphan@t.dev')`,
		// Affiliate 1 sold in dollars and yen; 2 in dollars only (an
		// imported sale: recorded on the 5th, sold on the 1st).
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks, conversions, commission_cents) VALUES
			(20260901, 1, 1, 10, 2, 1500), (20260902, 1, 1, 0, 1, 2000),
			(20260901, 2, 1, 5, 1, 700), (20260903, 3, 1, 4, 0, 0), (20260904, 4, 1, 0, 1, 300)`,
		`INSERT INTO token_stats_daily (day, affiliate_id, campaign_id, dim, value, clicks, conversions, commission_cents) VALUES
			(20260901, 1, 1, 'placement', 'sidebar', 10, 0, 0), (20260901, 2, 1, 'placement', 'hero', 5, 1, 700)`,
	} {
		if _, err := h.DB.Exec(s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	for i, c := range []struct {
		aff, amount   int64
		cur, at, snap string
	}{
		{1, 500, "USD", "2026-09-01T10:00:00Z", "{}"},
		{1, 1000, "JPY", "2026-09-01T11:00:00Z", "{}"},
		{1, 2000, "USD", "2026-09-02T09:00:00Z", "{}"},
		{1, 300, "USD", "2026-09-03T09:00:00Z", `{"origin":"usage","unit_type":"api"}`}, // usage: never in daily_agg
		{2, 700, "USD", "2026-09-05T09:00:00Z", "{}"},
	} {
		var jid int64
		if err := h.DB.QueryRow(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', ?) RETURNING id`,
			fmt.Sprintf("agg%d", i), c.at).Scan(&jid); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, created_at)
			VALUES (?, 1, ?, ?, ?, ?, 'pending', ?)`, c.aff, jid, c.amount, c.cur, c.snap, c.at); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string][3]int64{
		"20260901/1/1/":    {10, 0, 0},
		"20260901/1/1/USD": {0, 1, 500},
		"20260901/1/1/JPY": {0, 1, 1000},
		"20260902/1/1/USD": {0, 1, 2000},
		"20260901/2/1/":    {5, 0, 0},
		"20260901/2/1/USD": {0, 1, 700},
		"20260903/3/1/":    {4, 0, 0},
		"20260904/4/1/EUR": {0, 1, 300},
	}
	wantTok := map[string][3]int64{
		"20260901/1/1/":    {10, 0, 0},
		"20260901/2/1/":    {5, 0, 0},
		"20260901/2/1/USD": {0, 1, 700},
	}
	for run := 1; run <= 2; run++ {
		if err := migrations.Apply(ctx, h.DB, dbtest.Engine()); err != nil {
			t.Fatalf("upgrade run %d: %v", run, err)
		}
		if got := readAgg(t, h.DB, "daily_agg"); !sameAgg(got, want) {
			t.Fatalf("run %d: daily_agg = %v\nwant %v", run, got, want)
		}
		if got := readAgg(t, h.DB, "token_stats_daily"); !sameAgg(got, wantTok) {
			t.Fatalf("run %d: token_stats_daily = %v\nwant %v", run, got, wantTok)
		}
	}
	// Money needs a currency: the schema refuses it in a click row.
	if _, err := h.DB.Exec(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, conversions, commission_cents)
		VALUES (20260910, 1, 1, 1, 100)`); err == nil {
		t.Fatal("daily_agg accepted money without a currency")
	}
}
