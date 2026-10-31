package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// The Overview chart series: n days ending today, oldest first, summed
// across partners and programs, with quiet days as zeros.
func TestDailySeries_Engines(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	now := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)
	// Clicks are in currency-less rows, money in one row per currency:
	// the ¥40,000 credited on the 30th is not in a dollar series.
	for _, q := range []string{
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260930, 1, 1, '', 10, 0, 0)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260930, 1, 1, 'USD', 0, 1, 500)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260930, 1, 1, 'JPY', 0, 1, 40000)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260930, 2, 1, '', 5, 0, 0)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260930, 2, 1, 'USD', 0, 1, 250)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260928, 1, 1, '', 7, 0, 0)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260926, 1, 1, '', 99, 0, 0)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260926, 1, 1, 'USD', 0, 9, 999)`,
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260925, 1, 1, '', 50, 0, 0)`, // outside the window
		`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents) VALUES (20260925, 1, 1, 'USD', 0, 5, 555)`,
	} {
		if _, err := h.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := dailySeries(ctx, h.DB, now, 5, 0, "USD")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-09-26 99 9 999 [USD:999]",
		"2026-09-27 0 0 0 []",
		"2026-09-28 7 0 0 []",
		"2026-09-29 0 0 0 []",
		"2026-09-30 15 3 750 [JPY:40000 USD:750]",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d days", len(got))
	}
	for i := range want {
		if s := dayString(got[i]); s != want[i] {
			t.Errorf("day %d = %s, want %s", i, s, want[i])
		}
	}
	// One partner only.
	got, _ = dailySeries(ctx, h.DB, now, 1, 2, "USD")
	if s := dayString(got[0]); s != "2026-09-30 5 1 250 [USD:250]" {
		t.Fatalf("partner 2 = %s", s)
	}
	// A yen series reads the yen rows.
	got, _ = dailySeries(ctx, h.DB, now, 1, 0, "JPY")
	if got[0].CommissionCents != 40000 {
		t.Fatalf("yen series = %s", dayString(got[0]))
	}
	// A window ending yesterday.
	got, _ = dailySeries(ctx, h.DB, now.AddDate(0, 0, -1), 3, 0, "USD")
	if got[0].Day != "2026-09-27" || got[1].Clicks != 7 || got[2].Clicks != 0 {
		t.Fatalf("shifted = %+v", got)
	}
}

// dayString renders a DayStat for comparison: day, clicks, conversions,
// series commission and the per-currency list.
func dayString(d DayStat) string {
	parts := make([]string, 0, len(d.Commissions))
	for _, c := range d.Commissions {
		parts = append(parts, fmt.Sprintf("%s:%d", c.Currency, c.AmountMinor))
	}
	return fmt.Sprintf("%s %d %d %d [%s]", d.Day, d.Clicks, d.Conversions, d.CommissionCents, strings.Join(parts, " "))
}
