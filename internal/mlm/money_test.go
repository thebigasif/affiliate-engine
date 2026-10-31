package mlm_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

// seedApproved inserts an approved commission of amount for affiliate 3.
func seedApproved(t *testing.T, h *db.Handles, key string, amount int64) {
	t.Helper()
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', ?)`,
		key, nowRFC()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (3, (SELECT id FROM journal_entries WHERE idem_key = ?), ?, '{}', 'approved', ?)`,
		key, amount, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func periodLines(t *testing.T, h *db.Handles) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM mlm_period_lines`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Fix 6: a negative tier rate rejects the run (no negative payouts).
func TestRunPeriod_NegativeRateRejected(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	if _, err := h.DB.Exec(`UPDATE mlm_tier_rates SET rate_bp = -3000 WHERE level = 1`); err != nil {
		t.Fatal(err)
	}
	seedApproved(t, h, "neg", 10000)
	now := time.Now().UTC()
	if _, err := e.RunPeriod(ctx, now.Add(-time.Hour), now.Add(time.Hour)); err == nil {
		t.Fatal("negative tier rate accepted")
	}
	if n := periodLines(t, h); n != 0 {
		t.Fatalf("period lines=%d want 0 (run rolled back)", n)
	}
}

// Fix 6: an unrepresentable split rejects the run instead of saturating
// to MaxInt64 cents.
func TestRunPeriod_OverflowRejected(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	if _, err := h.DB.Exec(`UPDATE mlm_tier_rates SET rate_bp = 20000 WHERE level = 1`); err != nil {
		t.Fatal(err)
	}
	seedApproved(t, h, "ovf", 4611686018427387904) // 2^62 x 200% = 2^63: not an int64
	now := time.Now().UTC()
	if _, err := e.RunPeriod(ctx, now.Add(-time.Hour), now.Add(time.Hour)); err == nil {
		t.Fatal("overflowing split accepted")
	}
	if n := periodLines(t, h); n != 0 {
		t.Fatalf("period lines=%d want 0 (no saturated line)", n)
	}
}
