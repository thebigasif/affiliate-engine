package commissions_test

import (
	"context"
	"testing"
	"time"
)

// Commission period (campaigns.commission_months): a customer earns its
// affiliate commission for N months from the binding, then stops; 0 means
// lifetime. Dual-engine (bound_at is datetime text on SQLite, timestamptz
// on PG).
func TestCommission_PeriodCap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	setMonths := func(m int) {
		t.Helper()
		if _, err := e.h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'default') ON CONFLICT (id) DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		if _, err := e.h.DB.Exec(`UPDATE campaigns SET commission_months = ? WHERE id = 1`, m); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	pay := func(id, customer string, at time.Time) int64 {
		t.Helper()
		ev := purchase(id, 10000)
		ev.CustomerExtID = customer
		ev.Method = "token"
		ev.OccurredAt = at
		cid, err := e.e.ProcessPurchase(ctx, ev)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return cid
	}
	note := func(id string) string {
		var n *string
		e.h.DB.QueryRow(`SELECT process_error FROM raw_events WHERE external_event_id = ?`, id).Scan(&n)
		if n == nil {
			return ""
		}
		return *n
	}

	setMonths(12)
	if pay("p-first", "cus_a", now) == 0 {
		t.Fatal("first sale did not credit")
	}
	if pay("p-m11", "cus_a", now.AddDate(0, 11, 0)) == 0 {
		t.Fatal("renewal in month 11 did not credit")
	}
	if cid := pay("p-m12", "cus_a", now.AddDate(0, 12, 1)); cid != 0 {
		t.Fatalf("renewal after 12 months credited commission %d", cid)
	}
	if n := note("p-m12"); n != "commission_period_ended" {
		t.Fatalf("skip note = %q", n)
	}

	// A binding made before the first payment (trial checkout, import)
	// starts the period: 13 months old means no commission at all.
	if _, err := e.h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_at, bound_method)
		VALUES (1, 'cus_old', 1, ?, 'import:rewardful')`, now.AddDate(0, -13, 0).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if cid := pay("p-old", "cus_old", now); cid != 0 {
		t.Fatalf("customer bound 13 months ago credited %d", cid)
	}

	// No customer id: cannot be followed across payments, never capped.
	if pay("p-anon", "", now.AddDate(0, 30, 0)) == 0 {
		t.Fatal("one-off sale without customer id was capped")
	}

	// 0 = lifetime.
	setMonths(0)
	if pay("p-m36", "cus_a", now.AddDate(0, 36, 0)) == 0 {
		t.Fatal("lifetime program stopped paying")
	}
}
