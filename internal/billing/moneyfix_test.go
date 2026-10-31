package billing_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// Money fixes from the billing audit, each pinned against the REAL engine
// (newRealMoney: affiliate 1, 10% purchase rate) unless it is about event
// mapping only.

// stripeEvent builds a signed-ready Stripe envelope.
func mfEvent(id, typ string, obj map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "type": typ, "data": map[string]any{"object": obj}})
	return b
}

func stripeSend(t *testing.T, a *billing.StripeAdapter, body []byte) {
	t.Helper()
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
}

// paymentCheckout is a paid payment-mode session for 10000 (+tax).
func mfCheckout(pi, invoiceID string, total, tax int64) map[string]any {
	o := map[string]any{
		"id": "cs_" + pi, "mode": "payment", "payment_status": "paid",
		"amount_total": total, "currency": "usd", "customer": "cus_m",
		"payment_intent": pi, "created": 1700000000,
		"metadata":      map[string]any{"aff_token": "tok"},
		"total_details": map[string]any{"amount_discount": 0, "amount_tax": tax},
	}
	if invoiceID != "" {
		o["invoice"] = invoiceID
	}
	return o
}

// stripeCharge is a charge.refunded object as webhooks deliver it since
// API 2022-11-15: no refunds list, only the cumulative amount_refunded.
func stripeCharge(pi string, amount, refunded int64) map[string]any {
	return map[string]any{
		"id": "ch_" + pi, "amount": amount, "amount_refunded": refunded,
		"currency": "usd", "customer": "cus_m", "payment_intent": pi,
		"created": 1700000500,
	}
}

func commissionCount(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM commissions WHERE parent_commission_id IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Bug 1: charge.refunded carries the CUMULATIVE amount_refunded. Two $30
// refunds on $100 must claw 60% in total, not 30% + 60%.
func TestStripe_PartialRefundsAreCumulative(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
	stripeSend(t, a, mfEvent("evt_c1", "checkout.session.completed", mfCheckout("pi_r1", "", 10000, 0)))

	stripeSend(t, a, mfEvent("evt_r1", "charge.refunded", stripeCharge("pi_r1", 10000, 3000)))
	if _, claw := clawbackFor(t, d); claw != -300 {
		t.Fatalf("after first refund: clawback=%d want -300", claw)
	}
	second := mfEvent("evt_r2", "charge.refunded", stripeCharge("pi_r1", 10000, 6000))
	stripeSend(t, a, second)
	if _, claw := clawbackFor(t, d); claw != -600 {
		t.Fatalf("after second refund: clawback=%d want -600 (60%% of 1000)", claw)
	}
	stripeSend(t, a, second) // replay
	// An older delivery arriving late claws nothing more.
	stripeSend(t, a, mfEvent("evt_r1b", "charge.refunded", stripeCharge("pi_r1", 10000, 3000)))
	if _, claw := clawbackFor(t, d); claw != -600 {
		t.Fatalf("after replay / stale delivery: clawback=%d want -600", claw)
	}
}

// Bug 2: a dispute's withdrawn funds claw the commission back like a
// refund; a duplicate delivery claws once; reinstated funds are ACKed.
func TestStripe_DisputeFundsWithdrawnClawsBack(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
	stripeSend(t, a, mfEvent("evt_c2", "checkout.session.completed", mfCheckout("pi_d1", "", 10000, 0)))
	// A $30 refund first, then the buyer disputes the remaining $70.
	stripeSend(t, a, mfEvent("evt_r3", "charge.refunded", stripeCharge("pi_d1", 10000, 3000)))
	dispute := map[string]any{
		"id": "dp_1", "amount": 7000, "currency": "usd", "charge": "ch_pi_d1",
		"payment_intent": "pi_d1", "status": "needs_response", "created": 1700000900,
	}
	stripeSend(t, a, mfEvent("evt_dp1", "charge.dispute.funds_withdrawn", dispute))
	if _, claw := clawbackFor(t, d); claw != -1000 {
		t.Fatalf("after dispute: clawback=%d want -1000 (whole commission)", claw)
	}
	stripeSend(t, a, mfEvent("evt_dp1b", "charge.dispute.funds_withdrawn", dispute))
	if _, claw := clawbackFor(t, d); claw != -1000 {
		t.Fatalf("after duplicate dispute event: clawback=%d want -1000", claw)
	}
	dispute["status"] = "won"
	stripeSend(t, a, mfEvent("evt_dp2", "charge.dispute.funds_reinstated", dispute))
}

// Bug 2 + 5: a partial dispute on a taxed sale prorates against the
// amount charged (tax included), like refunds do.
func TestStripe_PartialDisputeOnTaxedSale(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
	stripeSend(t, a, mfEvent("evt_c3", "checkout.session.completed", mfCheckout("pi_d2", "", 11000, 1000)))
	stripeSend(t, a, mfEvent("evt_dp3", "charge.dispute.funds_withdrawn", map[string]any{
		"id": "dp_3", "amount": 5500, "currency": "usd", "charge": "ch_pi_d2", "payment_intent": "pi_d2",
	}))
	if _, claw := clawbackFor(t, d); claw != -500 {
		t.Fatalf("clawback=%d want -500 (half of the 1000 commission)", claw)
	}
}

// Bug 3: API 2025-03-31 (basil) dropped invoice.payment_intent and
// charge.invoice. A refund (found by pi_/ch_) must still reach the
// commission credited for the invoice — via invoice.payments when the
// payload includes it, or via invoice_payment.paid in either order.
func TestStripe_BasilInvoiceRefundLinks(t *testing.T) {
	basilInvoice := func(id string, payments []any) map[string]any {
		o := map[string]any{
			"id": id, "amount_paid": 10000, "total_excluding_tax": 10000, "currency": "usd",
			"customer": "cus_b", "billing_reason": "subscription_create", "created": 1700000100,
			"parent": map[string]any{"subscription_details": map[string]any{
				"subscription": "sub_b", "metadata": map[string]any{"aff_token": "tok"},
			}},
		}
		if payments != nil {
			o["payments"] = map[string]any{"data": payments}
		}
		return o
	}
	invPayment := func(inv, pi string) map[string]any {
		return map[string]any{
			"id": "inpay_" + pi, "object": "invoice_payment", "invoice": inv, "status": "paid",
			"payment": map[string]any{"type": "payment_intent", "payment_intent": pi},
		}
	}
	refund := func(pi string) map[string]any {
		c := stripeCharge(pi, 10000, 10000)
		return c // basil: no "invoice" on the charge
	}

	t.Run("payments included", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		stripeSend(t, a, mfEvent("evt_i1", "invoice.paid", basilInvoice("in_b1", []any{invPayment("in_b1", "pi_b1")})))
		stripeSend(t, a, mfEvent("evt_ir1", "charge.refunded", refund("pi_b1")))
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
	t.Run("invoice_payment.paid after the invoice", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		stripeSend(t, a, mfEvent("evt_i2", "invoice.paid", basilInvoice("in_b2", nil)))
		stripeSend(t, a, mfEvent("evt_ip2", "invoice_payment.paid", invPayment("in_b2", "pi_b2")))
		stripeSend(t, a, mfEvent("evt_ir2", "charge.refunded", refund("pi_b2")))
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
	t.Run("invoice_payment.paid before the invoice", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		stripeSend(t, a, mfEvent("evt_ip3", "invoice_payment.paid", invPayment("in_b3", "pi_b3")))
		stripeSend(t, a, mfEvent("evt_i3", "invoice.paid", basilInvoice("in_b3", nil)))
		stripeSend(t, a, mfEvent("evt_ir3", "charge.refunded", refund("pi_b3")))
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
}

// Bug 4: a payment-mode Checkout with invoice_creation sends
// checkout.session.completed AND invoice.paid (billing_reason=manual) for
// the same PaymentIntent. Exactly one commission, whichever lands first,
// on both invoice shapes (with payment_intent, and basil without it).
func TestStripe_CheckoutInvoiceCreationCreditsOnce(t *testing.T) {
	manual := func(pi string, withPI bool) map[string]any {
		o := map[string]any{
			"id": "in_" + pi, "amount_paid": 10000, "total_excluding_tax": 10000, "currency": "usd",
			"customer": "cus_m", "billing_reason": "manual", "created": 1700000001,
		}
		if withPI {
			o["payment_intent"] = pi
		}
		return o
	}
	for _, tc := range []struct {
		name          string
		invoiceFirst  bool
		invoiceHasPI  bool
		checkoutHasIn bool
	}{
		{"checkout first", false, true, true},
		{"invoice first", true, true, true},
		{"checkout first, no invoice id on session", false, true, false},
		{"invoice first, no invoice id on session", true, true, false},
		{"basil invoice, checkout first", false, false, true},
		{"basil invoice, invoice first", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, d := newRealMoney(t)
			a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
			inID := ""
			if tc.checkoutHasIn {
				inID = "in_pi_x"
			}
			co := mfEvent("evt_cx", "checkout.session.completed", mfCheckout("pi_x", inID, 10000, 0))
			inv := mfEvent("evt_ix", "invoice.paid", manual("pi_x", tc.invoiceHasPI))
			if tc.invoiceFirst {
				stripeSend(t, a, inv)
				stripeSend(t, a, co)
			} else {
				stripeSend(t, a, co)
				stripeSend(t, a, inv)
			}
			if n := commissionCount(t, d); n != 1 {
				t.Fatalf("commissions=%d want 1", n)
			}
		})
	}
}

// Bug 5: commission is on the tax-exclusive amount everywhere, and a
// refund prorates against what was charged (tax included), so half the
// money back claws half the commission.
func TestCommissionExcludesTax(t *testing.T) {
	ctx := context.Background()
	t.Run("stripe checkout", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		stripeSend(t, a, mfEvent("evt_t1", "checkout.session.completed", mfCheckout("pi_t1", "", 11000, 1000)))
		stripeSend(t, a, mfEvent("evt_t1r", "charge.refunded", stripeCharge("pi_t1", 11000, 5500)))
		assertCommissionAndClaw(t, d, 1000, -500)
	})
	t.Run("chargebee", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewChargebeeAdapter("cb_test_secret", engine, mockResolving(1, 1), store)
		send := func(b []byte) {
			if err := a.HandleWebhook(ctx, b, cbHeaders(signCB("cb_test_secret", b))); err != nil {
				t.Fatal(err)
			}
		}
		send(cbEnvelope("payment_succeeded", "evt_cbt", map[string]any{"invoice": map[string]any{
			"id": "inv_t", "customer_id": "c", "currency_code": "USD", "total": 11000, "tax": 1000, "status": "paid",
		}}))
		send(cbEnvelope("credit_note_created", "evt_cbt2", map[string]any{"credit_note": map[string]any{
			"id": "cn_t", "reference_invoice_id": "inv_t", "type": "refundable", "currency_code": "USD", "total": 5500,
		}}))
		assertCommissionAndClaw(t, d, 1000, -500)
	})
	t.Run("recurly", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewRecurlyAdapter("u", "p", "", engine, mockResolving(1, 1), store)
		inv := recurlyInvoiceXMLTax("inv-t", 11000, 1000)
		if err := a.HandleWebhook(ctx, inv, recurlyHeaders("u", "p")); err != nil {
			t.Fatal(err)
		}
		ref := recurlyRefundXML("successful_refund_notification", "tx-t", "inv-t", 5500)
		if err := a.HandleWebhook(ctx, ref, recurlyHeaders("u", "p")); err != nil {
			t.Fatal(err)
		}
		assertCommissionAndClaw(t, d, 1000, -500)
	})
	t.Run("lemon squeezy", func(t *testing.T) {
		engine, store, d := newRealMoney(t)
		a := billing.NewLemonSqueezyAdapter("ls_test_secret", engine, mockResolving(1, 1), store)
		attrs := lsOrderAttrs("2001", 11000, 0, nil)
		attrs["subtotal"], attrs["tax"] = int64(10000), int64(1000)
		lsSend(t, a, lsEnvelope("order_created", map[string]any{"id": "2001", "attributes": attrs}))
		attrs = lsOrderAttrs("2001", 11000, 5500, nil)
		attrs["subtotal"], attrs["tax"] = int64(10000), int64(1000)
		lsSend(t, a, lsEnvelope("order_refunded", map[string]any{"id": "2001", "attributes": attrs}))
		assertCommissionAndClaw(t, d, 1000, -500)
	})
}

func assertCommissionAndClaw(t *testing.T, d *sql.DB, wantAmount, wantClaw int64) {
	t.Helper()
	cid, claw := clawbackFor(t, d)
	var amount int64
	if err := d.QueryRow(`SELECT amount FROM commissions WHERE id = ?`, cid).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != wantAmount || claw != wantClaw {
		t.Fatalf("commission=%d clawback=%d, want %d and %d", amount, claw, wantAmount, wantClaw)
	}
}

func recurlyInvoiceXMLTax(uuid string, total, tax int64) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<closed_invoice_notification>
  <account><account_code>acct-t</account_code><email>t@example.com</email></account>
  <invoice>
    <uuid>` + uuid + `</uuid>
    <state>collected</state>
    <origin>purchase</origin>
    <invoice_number type="integer">1000</invoice_number>
    <subtotal_in_cents type="integer">` + itoa(total-tax) + `</subtotal_in_cents>
    <tax_in_cents type="integer">` + itoa(tax) + `</tax_in_cents>
    <total_in_cents type="integer">` + itoa(total) + `</total_in_cents>
    <currency>USD</currency>
    <closed_at type="datetime">2014-01-01T20:24:02Z</closed_at>
  </invoice>
</closed_invoice_notification>`)
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// flakyLinkStore fails the first LinkCommission call, like a database
// blip right after the credit.
type flakyLinkStore struct {
	*billing.SQLStore
	failed bool
}

func (f *flakyLinkStore) LinkCommission(ctx context.Context, source, eventID string, commissionID int64, keys ...string) error {
	if !f.failed {
		f.failed = true
		return errors.New("link: database is locked")
	}
	return f.SQLStore.LinkCommission(ctx, source, eventID, commissionID, keys...)
}

// Bug 6: a refund-link write that fails after the credit must not leave
// the commission unclawable. The webhook errors (the provider retries) and
// the retried delivery writes the link, so the refund claws.
func TestRefundLinkFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	t.Run("stripe", func(t *testing.T) {
		engine, sqlStore, d := newRealMoney(t)
		store := &flakyLinkStore{SQLStore: sqlStore}
		a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), store)
		co := mfEvent("evt_l1", "checkout.session.completed", mfCheckout("pi_l1", "", 10000, 0))
		if err := a.HandleWebhook(ctx, co, signedHeaders(co)); err == nil {
			t.Fatal("link failure was ACKed: Stripe would never retry it")
		}
		stripeSend(t, a, co) // Stripe's retry
		stripeSend(t, a, mfEvent("evt_l1r", "charge.refunded", stripeCharge("pi_l1", 10000, 10000)))
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
	t.Run("paddle", func(t *testing.T) {
		engine, sqlStore, d := newRealMoney(t)
		store := &flakyLinkStore{SQLStore: sqlStore}
		a := billing.NewPaddleAdapter("whsec_paddle_test", engine, mockResolving(1, 1), store)
		tx := paddleEnvelope("transaction.completed", "evt_pl1", map[string]any{
			"id": "txn_l1", "customer_id": "ctm_1", "currency_code": "USD",
			"details": map[string]any{"totals": map[string]any{"total": "10000", "tax": "0"}},
		})
		hdr := func(b []byte) map[string][]string {
			return paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), b))
		}
		if err := a.HandleWebhook(ctx, tx, hdr(tx)); err == nil {
			t.Fatal("link failure was ACKed: Paddle would never retry it")
		}
		paddleSend(t, a, tx)
		paddleSend(t, a, paddleEnvelope("adjustment.created", "evt_pl2", map[string]any{
			"id": "adj_l1", "transaction_id": "txn_l1", "action": "refund", "status": "approved",
			"currency_code": "USD", "totals": map[string]any{"total": "-10000", "tax": "0"},
		}))
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
	t.Run("chargebee", func(t *testing.T) {
		engine, sqlStore, d := newRealMoney(t)
		store := &flakyLinkStore{SQLStore: sqlStore}
		a := billing.NewChargebeeAdapter("cb_test_secret", engine, mockResolving(1, 1), store)
		inv := cbEnvelope("payment_succeeded", "evt_cbl", map[string]any{"invoice": map[string]any{
			"id": "inv_l", "customer_id": "c", "currency_code": "USD", "total": 10000, "status": "paid",
		}})
		if err := a.HandleWebhook(ctx, inv, cbHeaders(signCB("cb_test_secret", inv))); err == nil {
			t.Fatal("link failure was ACKed")
		}
		if err := a.HandleWebhook(ctx, inv, cbHeaders(signCB("cb_test_secret", inv))); err != nil {
			t.Fatal(err)
		}
		cn := cbEnvelope("credit_note_created", "evt_cbl2", map[string]any{"credit_note": map[string]any{
			"id": "cn_l", "reference_invoice_id": "inv_l", "type": "refundable", "currency_code": "USD", "total": 10000,
		}})
		if err := a.HandleWebhook(ctx, cn, cbHeaders(signCB("cb_test_secret", cn))); err != nil {
			t.Fatal(err)
		}
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
	t.Run("lemon squeezy", func(t *testing.T) {
		engine, sqlStore, d := newRealMoney(t)
		store := &flakyLinkStore{SQLStore: sqlStore}
		a := billing.NewLemonSqueezyAdapter("ls_test_secret", engine, mockResolving(1, 1), store)
		order := lsEnvelope("order_created", map[string]any{"id": "3001", "attributes": lsOrderAttrs("3001", 10000, 0, nil)})
		if err := a.HandleWebhook(ctx, order, lsHeaders(signLS("ls_test_secret", order))); err == nil {
			t.Fatal("link failure was ACKed")
		}
		lsSend(t, a, order)
		lsSend(t, a, lsEnvelope("order_refunded", map[string]any{"id": "3001", "attributes": lsOrderAttrs("3001", 10000, 10000, nil)}))
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
	t.Run("polar", func(t *testing.T) {
		engine, sqlStore, d := newRealMoney(t)
		store := &flakyLinkStore{SQLStore: sqlStore}
		a := billing.NewPolarAdapter(polarSecret, engine, mockResolving(1, 1), store)
		paid := polarOrder("order.paid", "po_l", 10000, 0, nil)
		if err := a.HandleWebhook(ctx, paid, signPolar(polarKey, "msg_l1", time.Now().Unix(), paid)); err == nil {
			t.Fatal("link failure was ACKed")
		}
		if err := a.HandleWebhook(ctx, paid, signPolar(polarKey, "msg_l1", time.Now().Unix(), paid)); err != nil {
			t.Fatal(err)
		}
		ref := polarOrder("order.refunded", "po_l", 10000, 10000, nil)
		if err := a.HandleWebhook(ctx, ref, signPolar(polarKey, "msg_l2", time.Now().Unix(), ref)); err != nil {
			t.Fatal(err)
		}
		if _, claw := clawbackFor(t, d); claw != -1000 {
			t.Fatalf("clawback=%d want -1000", claw)
		}
	})
}

// Bug 7: the sale time is the provider's event time, not the moment the
// webhook happens to be processed (a retry after an outage would judge
// token expiry and the commission period against the wrong day).
func TestOccurredAtIsProviderTime(t *testing.T) {
	ctx := context.Background()
	t.Run("paddle", func(t *testing.T) {
		a, mock, _ := newPaddleAdapter(t)
		b, _ := json.Marshal(map[string]any{
			"event_id": "evt_ot", "event_type": "transaction.completed", "occurred_at": "2026-09-01T10:00:00.123Z",
			"data": map[string]any{
				"id": "txn_ot", "customer_id": "ctm", "currency_code": "USD",
				"details": map[string]any{"totals": map[string]any{"total": "1000", "tax": "0"}},
			},
		})
		paddleSend(t, a, b)
		wantTime(t, mock.purchases[0].OccurredAt, "2026-09-01T10:00:00.123Z")
	})
	t.Run("chargebee", func(t *testing.T) {
		a, mock, _ := newCBAdapter()
		b, _ := json.Marshal(map[string]any{
			"id": "evt_cbot", "event_type": "payment_succeeded", "occurred_at": 1788256800, // 2026-09-01T10:00:00Z
			"content": map[string]any{"invoice": map[string]any{
				"id": "inv_ot", "customer_id": "c", "currency_code": "USD", "total": 1000, "status": "paid",
			}},
		})
		if err := a.HandleWebhook(ctx, b, cbHeaders(signCB("cb_test_secret", b))); err != nil {
			t.Fatal(err)
		}
		wantTime(t, mock.purchases[0].OccurredAt, "2026-09-01T10:00:00Z")
	})
	t.Run("lemon squeezy", func(t *testing.T) {
		a, mock, _ := newLSAdapter()
		attrs := lsOrderAttrs("4001", 1000, 0, nil)
		attrs["created_at"] = "2026-09-01T10:00:00.000000Z"
		lsSend(t, a, lsEnvelope("order_created", map[string]any{"id": "4001", "attributes": attrs}))
		wantTime(t, mock.purchases[0].OccurredAt, "2026-09-01T10:00:00Z")
	})
	t.Run("polar", func(t *testing.T) {
		a, mock, _, _ := newPolar()
		paid := polarOrder("order.paid", "po_ot", 1000, 0, nil) // timestamp 2026-09-30T10:00:00Z
		if err := a.HandleWebhook(ctx, paid, signPolar(polarKey, "msg_ot", time.Now().Unix(), paid)); err != nil {
			t.Fatal(err)
		}
		wantTime(t, mock.purchases[0].OccurredAt, "2026-09-30T10:00:00Z")
	})
}

func wantTime(t *testing.T, got time.Time, want string) {
	t.Helper()
	w, err := time.Parse(time.RFC3339Nano, want)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(w) {
		t.Fatalf("OccurredAt=%s want %s", got.Format(time.RFC3339Nano), want)
	}
}
