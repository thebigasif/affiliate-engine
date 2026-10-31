package mcp_test

import (
	"strings"
	"testing"
)

// Money is never summed across currencies: ¥5,000 and $12.00 are two
// totals, not "$62.00" (or 6200 "cents").
func TestMCP_PerCurrencyTotals(t *testing.T) {
	srv, d := newServerDB(t)
	if _, err := d.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('mj2','conversion', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, currency, rate_snapshot, status, created_at)
		VALUES (1, (SELECT id FROM journal_entries WHERE idem_key = 'mj2'), 5000, 'JPY', '{}', 'approved', ?)`, nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE commissions SET created_at = ? WHERE currency = 'USD'`, nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	text := func(name string, args map[string]any) string {
		out := call(t, srv, 1, "tools/call", map[string]any{"name": name, "arguments": args})
		return out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}

	earn := text("earnings_summary", map[string]any{"affiliate_id": 1})
	for _, want := range []string{"USD", "approved_cents=1200", "JPY", "approved_cents=5000"} {
		if !strings.Contains(earn, want) {
			t.Fatalf("earnings missing %q: %s", want, earn)
		}
	}
	if strings.Contains(earn, "6200") {
		t.Fatalf("earnings summed yen and cents: %s", earn)
	}

	dig := text("program_digest", map[string]any{"days": 7})
	for _, want := range []string{"$12.00", "5,000 JPY"} {
		if !strings.Contains(dig, want) {
			t.Fatalf("digest missing %q:\n%s", want, dig)
		}
	}
	if strings.Contains(dig, "$62.00") {
		t.Fatalf("digest summed yen and cents:\n%s", dig)
	}
}
