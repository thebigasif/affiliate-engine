package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// A refund that arrives before its purchase used to be recorded as a miss
// and never read again: the commission was then paid in full. It is now
// held, and the purchase applies it when it links the refund's keys. All
// tests run the real engine (newRealMoney: 10% of 10000 = 1000).

func TestHeldRefund_Stripe(t *testing.T) {
	checkout := mfEvent("evt_hc1", "checkout.session.completed", mfCheckout("pi_h1", "", 10000, 0))
	for _, tc := range []struct {
		name     string
		refunded int64
		wantClaw int64
	}{
		{"full refund", 10000, -1000},
		{"partial refund", 3000, -300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, d := newRealMoney(t)
			a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
			refund := mfEvent("evt_hr1", "charge.refunded", stripeCharge("pi_h1", 10000, tc.refunded))
			stripeSend(t, a, refund)
			if n := commissionCount(t, d); n != 0 {
				t.Fatalf("commissions before the purchase=%d want 0", n)
			}
			stripeSend(t, a, checkout)
			if _, claw := clawbackFor(t, d); claw != tc.wantClaw {
				t.Fatalf("clawback=%d want %d", claw, tc.wantClaw)
			}
			// Replaying either event changes nothing.
			stripeSend(t, a, refund)
			stripeSend(t, a, checkout)
			if _, claw := clawbackFor(t, d); claw != tc.wantClaw {
				t.Fatalf("after replays: clawback=%d want %d", claw, tc.wantClaw)
			}
			if n := commissionCount(t, d); n != 1 {
				t.Fatalf("commissions=%d want 1", n)
			}
		})
	}
	t.Run("later refund of the same charge claws only the rest", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		stripeSend(t, a, mfEvent("evt_hr2", "charge.refunded", stripeCharge("pi_h1", 10000, 3000)))
		stripeSend(t, a, checkout)
		stripeSend(t, a, mfEvent("evt_hr3", "charge.refunded", stripeCharge("pi_h1", 10000, 6000)))
		if _, claw := clawbackFor(t, d); claw != -600 {
			t.Fatalf("clawback=%d want -600", claw)
		}
	})
	t.Run("dispute before the purchase", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		stripeSend(t, a, mfEvent("evt_hd1", "charge.dispute.funds_withdrawn", map[string]any{
			"id": "dp_h1", "amount": 10000, "currency": "usd", "charge": "ch_pi_h1", "payment_intent": "pi_h1",
		}))
		stripeSend(t, a, checkout)
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
}

// API 2025-03-31+: the refund names only the pi_; the invoice credit
// links only the in_. invoice_payment.paid joins them, in any order.
func TestHeldRefund_StripeBasilInvoice(t *testing.T) {
	inv := mfEvent("evt_hbi", "invoice.paid", map[string]any{
		"id": "in_hb", "amount_paid": 10000, "total_excluding_tax": 10000, "currency": "usd",
		"customer": "cus_hb", "billing_reason": "subscription_create", "created": 1700000100,
		"parent": map[string]any{"subscription_details": map[string]any{
			"subscription": "sub_hb", "metadata": map[string]any{"aff_token": "tok"},
		}},
	})
	ip := mfEvent("evt_hbp", "invoice_payment.paid", map[string]any{
		"id": "inpay_hb", "object": "invoice_payment", "invoice": "in_hb", "status": "paid",
		"payment": map[string]any{"type": "payment_intent", "payment_intent": "pi_hb"},
	})
	refund := mfEvent("evt_hbr", "charge.refunded", stripeCharge("pi_hb", 10000, 10000))
	for _, tc := range []struct {
		name  string
		order [][]byte
	}{
		{"refund, payment, invoice", [][]byte{refund, ip, inv}},
		{"refund, invoice, payment", [][]byte{refund, inv, ip}},
		{"payment, refund, invoice", [][]byte{ip, refund, inv}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, d := newRealMoney(t)
			a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
			for _, b := range tc.order {
				stripeSend(t, a, b)
			}
			for _, b := range tc.order {
				stripeSend(t, a, b) // replays
			}
			if _, claw := clawbackFor(t, d); claw != -1000 {
				t.Fatalf("clawback=%d want -1000", claw)
			}
		})
	}
}

func TestHeldRefund_Paddle(t *testing.T) {
	tx := paddleEnvelope("transaction.completed", "evt_hp1", map[string]any{
		"id": "txn_h1", "customer_id": "ctm_1", "currency_code": "USD",
		"details": map[string]any{"totals": map[string]any{"total": "10000", "tax": "0"}},
	})
	adjustment := func(id, total, currency string) []byte {
		return paddleEnvelope("adjustment.created", "evt_"+id, map[string]any{
			"id": id, "transaction_id": "txn_h1", "action": "refund", "status": "approved",
			"currency_code": currency, "totals": map[string]any{"total": total, "tax": "0"},
		})
	}
	for _, tc := range []struct {
		name     string
		total    string
		currency string
		wantClaw int64
	}{
		{"full refund", "-10000", "USD", -1000},
		{"partial refund", "-5000", "USD", -500},
		{"other currency is skipped", "-10000", "EUR", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, d := newRealMoney(t)
			a := billing.NewPaddleAdapter("whsec_paddle_test", engine, mockResolving(1, 1), store)
			adj := adjustment("adj_h1", tc.total, tc.currency)
			paddleSend(t, a, adj)
			paddleSend(t, a, tx)
			paddleSend(t, a, tx)
			if tc.wantClaw != 0 {
				// A refund delivered again after the purchase takes the
				// direct path, which does not check the currency.
				paddleSend(t, a, adj)
			}
			if _, claw := clawbackFor(t, d); claw != tc.wantClaw {
				t.Fatalf("clawback=%d want %d", claw, tc.wantClaw)
			}
		})
	}
	t.Run("two refunds held under one transaction", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewPaddleAdapter("whsec_paddle_test", engine, mockResolving(1, 1), store)
		paddleSend(t, a, adjustment("adj_h2", "-2000", "USD"))
		paddleSend(t, a, adjustment("adj_h3", "-3000", "USD"))
		paddleSend(t, a, tx)
		if _, claw := clawbackFor(t, d); claw != -500 {
			t.Fatalf("clawback=%d want -500", claw)
		}
	})
}

func TestHeldRefund_LemonSqueezy(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewLemonSqueezyAdapter("ls_test_secret", engine, mockResolving(1, 1), store)
	refund := lsEnvelope("order_refunded", map[string]any{"id": "3101", "attributes": lsOrderAttrs("3101", 10000, 4000, nil)})
	order := lsEnvelope("order_created", map[string]any{"id": "3101", "attributes": lsOrderAttrs("3101", 10000, 0, nil)})
	lsSend(t, a, refund)
	lsSend(t, a, order)
	lsSend(t, a, refund)
	lsSend(t, a, order)
	if _, claw := clawbackFor(t, d); claw != -400 {
		t.Fatalf("clawback=%d want -400", claw)
	}
}

func TestHeldRefund_Polar(t *testing.T) {
	ctx := context.Background()
	engine, store, d := newRealMoney(t)
	a := billing.NewPolarAdapter(polarSecret, engine, mockResolving(1, 1), store)
	send := func(id string, body []byte) {
		t.Helper()
		if err := a.HandleWebhook(ctx, body, signPolar(polarKey, id, time.Now().Unix(), body)); err != nil {
			t.Fatal(err)
		}
	}
	send("msg_h1", polarOrder("order.refunded", "po_h", 10000, 10000, nil))
	send("msg_h2", polarOrder("order.paid", "po_h", 10000, 0, nil))
	if _, claw := clawbackFor(t, d); claw != -1000 {
		t.Fatalf("clawback=%d want -1000", claw)
	}
}

func TestHeldRefund_Chargebee(t *testing.T) {
	ctx := context.Background()
	engine, store, d := newRealMoney(t)
	a := billing.NewChargebeeAdapter("cb_test_secret", engine, mockResolving(1, 1), store)
	send := func(body []byte) {
		t.Helper()
		if err := a.HandleWebhook(ctx, body, cbHeaders(signCB("cb_test_secret", body))); err != nil {
			t.Fatal(err)
		}
	}
	cn := cbEnvelope("credit_note_created", "evt_hcb2", map[string]any{"credit_note": map[string]any{
		"id": "cn_h", "reference_invoice_id": "inv_h", "type": "refundable", "currency_code": "USD", "total": 2500,
	}})
	inv := cbEnvelope("payment_succeeded", "evt_hcb1", map[string]any{"invoice": map[string]any{
		"id": "inv_h", "customer_id": "c", "currency_code": "USD", "total": 10000, "status": "paid",
	}})
	send(cn)
	send(inv)
	send(cn)
	if _, claw := clawbackFor(t, d); claw != -250 {
		t.Fatalf("clawback=%d want -250", claw)
	}
}

// A purchase with no earlier refund is untouched, even when refunds of
// other payments are held.
func TestHeldRefund_OtherPurchaseUntouched(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
	stripeSend(t, a, mfEvent("evt_ho1", "charge.refunded", stripeCharge("pi_other", 10000, 10000)))
	stripeSend(t, a, mfEvent("evt_ho2", "checkout.session.completed", mfCheckout("pi_mine", "", 10000, 0)))
	if _, claw := clawbackFor(t, d); claw != 0 {
		t.Fatalf("clawback=%d want 0", claw)
	}
}
