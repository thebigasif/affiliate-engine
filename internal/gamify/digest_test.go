package gamify_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// D1: digest derives from seeded facts, flags holds, money formatting.
func TestDigest(t *testing.T) {
	h, e := newEnv(t)
	ctx := context.Background()
	day := time.Now().UTC().Format("20060102")
	var d int64
	for _, c := range day {
		d = d*10 + int64(c-'0')
	}
	h.DB.Exec(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents)
		VALUES (?, 1, 1, '', 1000, 0, 0), (?, 1, 1, 'USD', 0, 25, 25000)`, d, d)
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('dj','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (1, 1, 25000, '{}', 'approved', ?)`, time.Now().UTC().Format(time.RFC3339))
	h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (1, 'review')`)

	out, err := e.GenerateDigest(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"25 conversions", "$250.00", "1 held commission", "balanced"} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "review the fraud queue") {
		t.Fatal("action hint missing when holds exist")
	}
}

// D2: empty program digests cleanly.
func TestDigestEmpty(t *testing.T) {
	_, e := newEnv(t)
	out, err := e.GenerateDigest(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0 conversions") || !strings.Contains(out, "balanced") {
		t.Fatalf("empty digest:\n%s", out)
	}
}
