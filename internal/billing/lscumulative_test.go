package billing_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// LS15: Lemon Squeezy reports the CUMULATIVE refunded total on every
// order_refunded delivery, so the adapter must use the engine's cumulative
// entry point. Real engine: 10% of 10000 = 1000 commission; cumulative
// refunds of 2500 then 6000 claw 600 in total (60%) — never 250 + 600.
func TestLemonSqueezy_CumulativeRefundsRealEngine(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewLemonSqueezyAdapter("ls_test_secret", engine, mockResolving(1, 1), store)
	send := func(body []byte) {
		t.Helper()
		if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
			t.Fatal(err)
		}
	}
	send(lsEnvelope("order_created", map[string]any{
		"id": "1080", "attributes": lsOrderAttrs("1080", 10000, 0, nil),
	}))

	send(lsEnvelope("order_refunded", map[string]any{
		"id": "1080", "attributes": lsOrderAttrs("1080", 10000, 2500, nil),
	}))
	if _, claw := clawbackFor(t, d); claw != -250 {
		t.Fatalf("after cumulative 2500: clawback=%d want -250", claw)
	}

	second := lsEnvelope("order_refunded", map[string]any{
		"id": "1080", "attributes": lsOrderAttrs("1080", 10000, 6000, nil),
	})
	send(second)
	if _, claw := clawbackFor(t, d); claw != -600 {
		t.Fatalf("after cumulative 6000: clawback=%d want -600 (60%% of 1000; the first refund must not be re-clawed)", claw)
	}

	// Replaying the second delivery changes nothing.
	send(second)
	if _, claw := clawbackFor(t, d); claw != -600 {
		t.Fatalf("after replay: clawback=%d want -600", claw)
	}
}
