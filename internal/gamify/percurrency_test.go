package gamify_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The digest reports commissions per currency: ¥5,000 and $10.00 are two
// totals, never "$60.00".
func TestDigest_PerCurrency(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for i, c := range []struct {
		aff, amount int64
		cur         string
	}{{1, 1000, "USD"}, {2, 5000, "JPY"}} {
		key := "pc" + fmtI(int64(i))
		if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', CURRENT_TIMESTAMP)`, key); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, currency, rate_snapshot, status, created_at)
			VALUES (?, (SELECT id FROM journal_entries WHERE idem_key = ?), ?, ?, '{}', 'approved', ?)`,
			c.aff, key, c.amount, c.cur, now); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.GenerateDigest(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$10.00", "5,000 JPY"} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$60.00") {
		t.Fatalf("digest summed yen and cents:\n%s", out)
	}
}
