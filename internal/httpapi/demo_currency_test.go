package httpapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// The demo's Portal tab shows the top earner in the program currency.
// ¥90,000 is a bigger number than $500.00 but not more money: yen and
// cents do not rank against each other, so the dollar earner is shown.
func TestDemoInfo_RanksInProgramCurrency(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email, status) VALUES (1, 'yen@x.test', 'active'), (2, 'usd@x.test', 'active')`); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		aff, amount int64
		cur         string
	}{{1, 90000, "JPY"}, {2, 50000, "USD"}, {1, 100, "USD"}} {
		var jid int64
		if err := h.DB.QueryRow(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', '2026-09-30T00:00:00Z') RETURNING id`,
			fmt.Sprintf("demo%d", i)).Scan(&jid); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, currency, rate_snapshot, status)
			VALUES (?, ?, ?, ?, '{}', 'approved')`, c.aff, jid, c.amount, c.cur); err != nil {
			t.Fatal(err)
		}
	}
	a := &api{db: h.DB}
	if got := a.demoInfo(ctx).AffiliateID; got != 2 {
		t.Fatalf("demo affiliate = %d, want 2 (top earner in USD, the program currency)", got)
	}
	// In a yen program the yen earner is the top one.
	if _, err := h.DB.Exec(`UPDATE program_settings SET currency = 'JPY' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := a.demoInfo(ctx).AffiliateID; got != 1 {
		t.Fatalf("demo affiliate = %d, want 1 (top earner in JPY)", got)
	}
}
