package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// Earnings, the ledger view and the dashboard report money per currency:
// a ¥8,000 approved commission sits beside affiliate 1's dollars, never
// in them. The single-number fields keep reporting dollars (the program
// currency) exactly as before.
func TestAPI_PerCurrencySummaries(t *testing.T) {
	f := setupAPI(t)
	ctx := context.Background()
	p := ledger.NewPoster(f.db.Ledger)
	jid, err := p.Post(ctx, "test:comm:jpy", "conversion", "", time.Now().UTC(), []ledger.Line{
		{Account: ledger.Expense("JPY"), Amount: 8000},
		{Account: ledger.Payable(1, "JPY"), Amount: -8000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB.Exec(`INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, created_at)
		VALUES (1, 1, ?, 8000, 'JPY', '{}', 'approved', ?)`, jid, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	code, e := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key)
	if code != 200 {
		t.Fatalf("earnings: %d %v", code, e)
	}
	// Legacy fields: dollars only (approved $30.00 + pending $20.00).
	if e["currency"] != "USD" || e["unpaid_cents"].(float64) != 5000 || e["payable_balance_cents"].(float64) != -3000 {
		t.Fatalf("legacy fields mixed currencies: currency=%v unpaid=%v payable=%v",
			e["currency"], e["unpaid_cents"], e["payable_balance_cents"])
	}
	if !e["ledger_proof"].(map[string]any)["holds"].(bool) {
		t.Fatalf("ledger proof: %v", e["ledger_proof"])
	}
	curs := e["currencies"].([]any)
	if len(curs) != 2 {
		t.Fatalf("currencies=%v want JPY and USD", curs)
	}
	jpy := curs[0].(map[string]any)
	if jpy["currency"] != "JPY" || jpy["unpaid_cents"].(float64) != 8000 ||
		jpy["payable_balance_cents"].(float64) != -8000 || !jpy["holds"].(bool) {
		t.Fatalf("JPY entry: %v", jpy)
	}

	code, l := getJSON(t, f.srv.URL+"/api/v1/affiliate/ledger?currency=jpy", f.aff1Key)
	if code != 200 || l["currency"] != "JPY" || len(l["entries"].([]any)) != 1 || len(l["currencies"].([]any)) != 2 {
		t.Fatalf("JPY ledger view: %d %v", code, l)
	}

	code, d := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.adminKey)
	if code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	if d["commissions_by_status_cents"].(map[string]any)["approved"].(float64) != 3000 {
		t.Fatalf("dashboard legacy approved mixes currencies: %v", d["commissions_by_status_cents"])
	}
	if n := len(d["commissions_by_currency"].([]any)); n != 2 {
		t.Fatalf("commissions_by_currency=%d want 2", n)
	}
	if d["ledger_integrity_sum"].(float64) != 0 {
		t.Fatalf("integrity: %v", d["ledger_integrity_by_currency"])
	}
}
