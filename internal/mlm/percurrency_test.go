package mlm_test

import (
	"context"
	"testing"
	"time"
)

// Business volume is counted in the program currency only (USD here),
// like bonus tier volume: a yen sale adds no BV, so ¥20,000 never counts
// as 20,000 toward a rank threshold set in cents. Nothing is converted.
func TestMLM_BVProgramCurrencyOnly(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	if err := e.RecordBV(ctx, 3, 20260915, "JPY", 20000); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordBV(ctx, 3, 20260915, "usd", 500); err != nil {
		t.Fatal(err)
	}
	var p3, g1 int64
	if err := h.DB.QueryRow(`SELECT bv_personal FROM mlm_volume_daily WHERE affiliate_id=3`).Scan(&p3); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.QueryRow(`SELECT bv_group FROM mlm_volume_daily WHERE affiliate_id=1`).Scan(&g1); err != nil {
		t.Fatal(err)
	}
	if p3 != 500 || g1 != 500 {
		t.Fatalf("bv personal3=%d group1=%d, want 500 (the yen sale adds none)", p3, g1)
	}
	// A yen program counts yen and ignores dollars.
	if _, err := h.DB.Exec(`UPDATE program_settings SET currency = 'JPY' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordBV(ctx, 3, 20260916, "JPY", 20000); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordBV(ctx, 3, 20260916, "USD", 500); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.QueryRow(`SELECT bv_personal FROM mlm_volume_daily WHERE affiliate_id=3 AND day=20260916`).Scan(&p3); err != nil {
		t.Fatal(err)
	}
	if p3 != 20000 {
		t.Fatalf("yen program bv=%d want 20000", p3)
	}
}

// Tier splits are in the currency of the commission they split: a period
// with a dollar and a yen commission has per-currency totals, never one
// number adding cents to yen.
func TestMLM_PeriodPerCurrency(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	for _, c := range []struct {
		key, cur string
		amount   int64
	}{{"pc-usd", "USD", 10000}, {"pc-jpy", "JPY", 50000}} {
		if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', ?)`, c.key, nowRFC()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, currency, rate_snapshot, status, created_at)
			VALUES (3, (SELECT id FROM journal_entries WHERE idem_key = ?), ?, ?, '{}', 'approved', ?)`,
			c.key, c.amount, c.cur, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := e.RunPeriod(ctx, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var periodID int64
	if err := h.DB.QueryRow(`SELECT id FROM mlm_payout_periods`).Scan(&periodID); err != nil {
		t.Fatal(err)
	}
	tot, err := e.PeriodTotal(ctx, periodID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tot) != 2 || tot["USD"] != 3000 || tot["JPY"] != 15000 {
		t.Fatalf("level-1 totals = %v, want USD 3000 and JPY 15000", tot)
	}
	var jpyLines int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM mlm_period_lines WHERE currency = 'JPY'`).Scan(&jpyLines); err != nil {
		t.Fatal(err)
	}
	if jpyLines != 2 {
		t.Fatalf("yen lines=%d want 2 (levels 1 and 2)", jpyLines)
	}
}
