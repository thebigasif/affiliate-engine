package migrations_test

import (
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// Both dialect chains: the file set comes from migrations.Names — the exact
// list Apply executes — so no script can escape validation (fraud2/tokenstats/
// dc once drifted out of a hand-maintained list). Scripts must exist, apply in
// order, never collide table names, and keep dialect tokens out of the wrong
// dialect.
func TestChainsWellFormed(t *testing.T) {
	for _, tc := range []struct {
		engine migrations.Engine
		bad    []string // tokens that must NOT appear
	}{
		{migrations.Postgres, []string{"AUTOINCREMENT", "CURRENT_TIMESTAMP"}},
		{migrations.SQLite, []string{"BIGSERIAL", "TIMESTAMPTZ", "::"}},
	} {
		names := migrations.Names(tc.engine)
		if len(names) == 0 {
			t.Fatalf("%s: empty migration list", tc.engine)
		}
		seen := map[string]bool{}
		for _, f := range names {
			b, err := migrations.Files().ReadFile(f)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			s := string(b)
			for _, bad := range tc.bad {
				if strings.Contains(s, bad) {
					t.Errorf("%s (%s): contains forbidden token %q", f, tc.engine, bad)
				}
			}
			for _, line := range strings.Split(s, "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "CREATE TABLE IF NOT EXISTS") {
					continue
				}
				fields := strings.Fields(trimmed)
				if len(fields) < 5 {
					continue
				}
				// Line shape: "CREATE TABLE IF NOT EXISTS name (" -> fields[5].
				if len(fields) < 6 {
					continue
				}
				name := strings.Trim(fields[5], "(")
				if name == "" {
					continue
				}
				if seen[name] {
					t.Errorf("%s: duplicate table %s", f, name)
				}
				seen[name] = true
			}
		}
		want := map[string]bool{
			"ledger_accounts": false, "ledger_entries": false, "journal_entries": false,
			"commissions": false, "clicks": false, "payouts": false,
			"fraud_verdicts": false, "api_keys": false, "affiliate_sessions": false, "affiliate_applications": false, "mlm_closure": false, "campaigns": false,
		}
		for k := range want {
			if !seen[k] {
				t.Errorf("%s chain: missing table %s", tc.engine, k)
			}
		}
	}
}
