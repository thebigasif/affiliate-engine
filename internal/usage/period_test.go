package usage_test

import (
	"context"
	"testing"
)

// Commission period on the metered path: usage on days after the
// program's commission_months (from the binding) earns nothing.
func TestUsage_PeriodCap(t *testing.T) {
	h, ing, set, _ := newEnv(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'default') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE campaigns SET commission_months = 12 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	// cus_ai (newEnv) was bound just now: in period. cus_old was bound
	// in January 2025, so September 2026 is past its 12 months.
	if _, err := h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_at)
		VALUES (1, 'cus_old', 1, '2025-01-10T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ id, customer string }{{"in-1", "cus_ai"}, {"old-1", "cus_old"}} {
		x := ev(e.id, 100000)
		x.CustomerExtID = e.customer
		if err := ing.Ingest(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	n, err := set.SettleDay(ctx, 20260915, 300)
	if err != nil || n != 1 {
		t.Fatalf("settle n=%d err=%v; want only the in-period customer", n, err)
	}
	var total int64
	h.DB.QueryRow(`SELECT SUM(amount) FROM commissions`).Scan(&total)
	if total != 3000 {
		t.Fatalf("commissions total=%d want 3000", total)
	}
}
