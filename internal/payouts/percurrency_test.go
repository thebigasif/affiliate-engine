package payouts_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// Minimum payouts are per currency: $50.00 does not mean ¥50, and a yen
// group is measured against the yen minimum only.
func TestPerCurrency_ThresholdPerCurrency(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	creditMature(t, e, h, mkEvent("th-usd", 1, 60000)) // $60.00
	jpy := mkEvent("th-jpy", 1, 40000)                 // ¥4,000
	jpy.Currency = "JPY"
	creditMature(t, e, h, jpy)

	th := payouts.Thresholds{Default: 5000, ByCurrency: map[string]int64{"JPY": 5000}}
	plan, err := r.PreviewWith(ctx, th)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Currency != "USD" || plan[0].Cents != 6000 || plan[0].Display != "$60.00" {
		t.Fatalf("plan=%+v want only the $60.00 USD payout (¥4,000 is under ¥5,000)", plan)
	}
	th.ByCurrency["JPY"] = 3000
	if _, n, err := r.CreateBatchWith(ctx, th, "manual"); err != nil || n != 2 {
		t.Fatalf("batch: n=%d err=%v want 2 (¥4,000 clears ¥3,000)", n, err)
	}
}

// A yen receivable (a refund after a JPY payout) must not block, or be
// netted against, a dollar payout. Balances were per affiliate: ¥20,000
// owed back plus $100.00 approved made one mixed balance of +10000, the
// USD payout was planned (per currency), the rail executed, and the
// settle then tripped the payable overdraft guard: payout 'failed' with
// the money already sent.
func TestPerCurrency_YenReceivableDoesNotFailDollarPayout(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)

	jpy := mkEvent("pc-jpy", 1, 200000) // ¥20,000 commission
	jpy.Currency = "JPY"
	cj := creditMature(t, e, h, jpy)
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("JPY batch: n=%d err=%v", n, err)
	}
	pj, _ := onlyPayout(t, h, "requested")
	if err := r.Approve(ctx, pj, nil); err != nil {
		t.Fatal(err)
	}
	// Full yen refund after payout: the affiliate owes ¥20,000 back.
	ref := refundOf("pc-jpy-ref")
	ref.Currency = "JPY"
	if err := e.ProcessRefund(ctx, ref, cj, 200000, 200000); err != nil {
		t.Fatal(err)
	}

	creditMature(t, e, h, mkEvent("pc-usd", 1, 100000)) // $100.00
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("USD batch: n=%d err=%v (want the USD payout only)", n, err)
	}
	pu, amount := onlyPayout(t, h, "requested")
	if amount != 10000 {
		t.Fatalf("USD payout=%d want 10000 (the yen receivable is not netted into dollars)", amount)
	}
	rail := 0
	if err := r.Approve(ctx, pu, func(int64, int64) error { rail++; return nil }); err != nil {
		t.Fatalf("USD payout after a yen receivable: %v (rail ran %d times)", err, rail)
	}
	var status string
	if err := h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pu).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Fatalf("USD payout status=%s want paid", status)
	}
}

// MO-9: one minimum typed in major units applies in each currency by its
// exponent. "50" is $50.00 (5000) and ¥50 (50), never ¥5,000; extra
// decimals are dropped for a currency without them.
func TestPerCurrency_MajorThresholds(t *testing.T) {
	th, err := payouts.MajorThresholds("50")
	if err != nil {
		t.Fatal(err)
	}
	for cur, want := range map[string]int64{"USD": 5000, "JPY": 50, "KWD": 50000, "EUR": 5000} {
		if got := th.For(cur); got != want {
			t.Errorf("%s: %d, want %d", cur, got, want)
		}
	}
	th, _ = payouts.MajorThresholds("12.345")
	if th.For("USD") != 1234 || th.For("JPY") != 12 || th.For("KWD") != 12345 {
		t.Errorf("12.345: USD %d JPY %d KWD %d", th.For("USD"), th.For("JPY"), th.For("KWD"))
	}
	for _, bad := range []string{"", "-5", "5,00", "1e3", "abc", "5."} {
		if _, err := payouts.MajorThresholds(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	h, r, e := newEnv(t)
	ctx := context.Background()
	creditMature(t, e, h, mkEvent("mj-usd", 1, 40000)) // $40.00
	jpy := mkEvent("mj-jpy", 1, 40000)                 // ¥4,000
	jpy.Currency = "JPY"
	creditMature(t, e, h, jpy)
	th, _ = payouts.MajorThresholds("50")
	plan, err := r.PreviewWith(ctx, th)
	if err != nil {
		t.Fatal(err)
	}
	// $40.00 is under $50.00; ¥4,000 clears ¥50 (the old x100 made it
	// ¥5,000 and held the yen payout back).
	if len(plan) != 1 || plan[0].Currency != "JPY" || plan[0].Cents != 4000 {
		t.Fatalf("plan=%+v want only the ¥4,000 payout", plan)
	}
}
