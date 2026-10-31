package usage_test

import (
	"context"
	"testing"
)

// Fix 6: a negative rate is rejected before any meter is touched.
func TestUsage_NegativeRateRejected(t *testing.T) {
	h, ing, set, _ := newEnv(t)
	ctx := context.Background()
	if err := ing.Ingest(ctx, ev("neg1", 100000)); err != nil {
		t.Fatal(err)
	}
	if _, err := set.SettleDay(ctx, 20260915, -300); err == nil {
		t.Fatal("negative rate accepted")
	}
	var n, j int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&j)
	if n != 0 || j != 0 {
		t.Fatalf("commissions=%d journals=%d want 0/0", n, j)
	}
}

// Fix 6: an unrepresentable basis is REJECTED — never saturated to
// MaxInt64 (a plausible-looking number that would pay real money) — and
// nothing is posted.
func TestUsage_OverflowRejectedNotSaturated(t *testing.T) {
	h, ing, set, poster := newEnv(t)
	ctx := context.Background()
	if err := ing.Ingest(ctx, ev("ov1", 8000000000000000000)); err != nil {
		t.Fatal(err)
	}
	// 8e18 units x 20000bp / 10000 = 1.6e19: not an int64.
	if _, err := set.SettleDay(ctx, 20260915, 20000); err == nil {
		t.Fatal("overflowing basis accepted")
	}
	var settled int64
	h.DB.QueryRow(`SELECT settled_cents FROM usage_daily WHERE customer_ext_id='cus_ai'`).Scan(&settled)
	var n, j int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&j)
	if settled != 0 || n != 0 || j != 0 {
		t.Fatalf("settled=%d commissions=%d journals=%d want 0/0/0", settled, n, j)
	}
	// A representable rate still settles exactly: 8e18 x 1 / 10000.
	got, err := set.SettleDay(ctx, 20260915, 1)
	if err != nil || got != 1 {
		t.Fatalf("settle n=%d err=%v", got, err)
	}
	var amount int64
	h.DB.QueryRow(`SELECT amount FROM commissions`).Scan(&amount)
	if amount != 800000000000000 {
		t.Fatalf("amount=%d want 800000000000000 (exact)", amount)
	}
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}
