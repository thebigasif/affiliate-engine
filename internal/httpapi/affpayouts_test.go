package httpapi

import (
	"fmt"
	"strings"
	"testing"
)

// A partner sees their own payouts, newest first, without failed ones and
// never another partner's.
func TestAffiliatePayouts(t *testing.T) {
	f := setupAPI(t)
	for _, q := range []string{
		`INSERT INTO payouts (affiliate_id, amount, currency, method, status, created_at, processed_at) VALUES (1, 5000, 'USD', 'paypal', 'paid', '2026-09-01T10:00:00Z', '2026-09-02T10:00:00Z')`,
		`INSERT INTO payouts (affiliate_id, amount, currency, method, status, created_at) VALUES (1, 700, 'USD', 'wise', 'failed', '2026-09-10T10:00:00Z')`,
		`INSERT INTO payouts (affiliate_id, amount, currency, method, status, created_at) VALUES (1, 3000, 'USD', 'wise', 'requested', '2026-09-20T10:00:00Z')`,
		`INSERT INTO payouts (affiliate_id, amount, currency, method, status, created_at) VALUES (2, 9900, 'USD', 'paypal', 'requested', '2026-09-20T10:00:00Z')`,
	} {
		if _, err := f.db.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/payouts", f.aff1Key)
	if code != 200 {
		t.Fatalf("code %d %v", code, m)
	}
	ps, _ := m["payouts"].([]any)
	var got []string
	for _, p := range ps {
		x := p.(map[string]any)
		got = append(got, fmt.Sprintf("%v/%v/%v", x["amount_cents"], x["status"], x["paid_at"] != nil))
	}
	// The fixture has its own paid payout for affiliate 1 too; ours are the newest.
	if len(got) < 2 || got[0] != "3000/requested/false" || got[1] != "5000/paid/true" {
		t.Fatalf("payouts = %v", got)
	}
	for _, g := range got {
		if strings.HasPrefix(g, "700/") || strings.HasPrefix(g, "9900/") {
			t.Fatalf("failed or someone else's payout listed: %v", got)
		}
	}
}
