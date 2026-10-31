package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
)

// TS1: rollup unpivots all three dims; cursor advances; re-run idempotent.
func TestTokenStats_Rollup(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("20060102")
	dayN := int64(0)
	for _, c := range today {
		dayN = dayN*10 + int64(c-'0')
	}
	// 3 clicks with distinct placements + utm + subid, 1 bot.
	for i, tc := range []struct{ utm, sub, place string }{
		{"nl", "a", "sidebar"},
		{"nl", "b", "sidebar"},
		{"twitter", "a", "hero"},
	} {
		if _, err := h.DB.Exec(`
			INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, utm_source, subid, placement, bot_class, exclude_stats)
			VALUES (?, ?, ?, ?, 1, 1, ?, ?, ?, 0, 0)`,
			fmt.Sprintf("t%02d", i), today+"T12:00:00Z", dayN, dayN/100, tc.utm, tc.sub, tc.place); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.Exec(`
		INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, utm_source, placement, bot_class, exclude_stats)
		VALUES ('tbot', ?, ?, ?, 1, 1, 'botutm', 'botplace', 2, 1)`,
		today+"T12:00:00Z", dayN, dayN/100); err != nil {
		t.Fatal(err)
	}

	ts := analytics.NewTokenStats(h.DB)
	n, err := ts.Rollup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no rows rolled")
	}
	// Placement drill: sidebar=2, hero=1; bot excluded.
	rows, err := ts.Drill(ctx, 1, "placement", dayN, dayN, "USD", 50)
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]int64{}
	for _, r := range rows {
		totals[r.Value] = r.Clicks
	}
	if totals["sidebar"] != 2 || totals["hero"] != 1 {
		t.Fatalf("placement: %v", totals)
	}
	for _, r := range rows {
		if r.Value == "botplace" {
			t.Fatal("bot rolled into token stats")
		}
	}
	// Re-run: idempotent (delete+reinsert per day).
	n2, err := ts.Rollup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows2, _ := ts.Drill(ctx, 1, "placement", dayN, dayN, "USD", 50)
	sum := int64(0)
	for _, r := range rows2 {
		sum += r.Clicks
	}
	if sum != 3 {
		t.Fatalf("after re-run sum=%d want 3 (idempotent)", sum)
	}
	_ = n2
}

// TS2: ApplyConversion credits day rows; drill shows conversions.
func TestTokenStats_Conversions(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("20060102")
	var dayN int64
	for _, c := range today {
		dayN = dayN*10 + int64(c-'0')
	}
	h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, placement)
		VALUES ('c1', ?, ?, ?, 1, 1, 'sidebar')`, today+"T12:00:00Z", dayN, dayN/100)
	ts := analytics.NewTokenStats(h.DB)
	if _, err := ts.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApplyConversion(ctx, dayN, 1, 1, "USD", 1500); err != nil {
		t.Fatal(err)
	}
	rows, _ := ts.Drill(ctx, 1, "placement", dayN, dayN, "USD", 10)
	if len(rows) != 1 || rows[0].Conversions != 1 || rows[0].CommissionCents != 1500 {
		t.Fatalf("drill: %+v", rows)
	}
}

// TS3: token money is per currency. A yen conversion on the same
// placement is its own row: commission_cents is the asked currency's,
// commissions lists each currency, and a re-rollup of the day keeps the
// money (it recomputes clicks only).
func TestTokenStats_PerCurrency(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("20060102")
	var dayN int64
	for _, c := range today {
		dayN = dayN*10 + int64(c-'0')
	}
	h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, placement)
		VALUES ('c1', ?, ?, ?, 1, 1, 'sidebar')`, today+"T12:00:00Z", dayN, dayN/100)
	ts := analytics.NewTokenStats(h.DB)
	if _, err := ts.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApplyConversion(ctx, dayN, 1, 1, "USD", 1500); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApplyConversion(ctx, dayN, 1, 1, "jpy", 2000); err != nil {
		t.Fatal(err)
	}
	h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, placement)
		VALUES ('c2', ?, ?, ?, 1, 1, 'sidebar')`, today+"T13:00:00Z", dayN, dayN/100)
	if _, err := ts.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := ts.Drill(ctx, 1, "placement", dayN, dayN, "USD", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("drill rows: %+v", rows)
	}
	r := rows[0]
	if r.Clicks != 2 || r.Conversions != 2 || r.CommissionCents != 1500 || r.Currency != "USD" {
		t.Fatalf("drill: %+v (want 2 clicks, 2 sales, 1500 USD)", r)
	}
	if len(r.Commissions) != 2 || r.Commissions[0].Currency != "JPY" || r.Commissions[0].AmountMinor != 2000 ||
		r.Commissions[1].Currency != "USD" || r.Commissions[1].AmountMinor != 1500 {
		t.Fatalf("drill commissions: %+v", r.Commissions)
	}
}
