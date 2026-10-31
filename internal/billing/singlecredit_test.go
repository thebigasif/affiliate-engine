package billing_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// One Stripe payment credits once. A payment-mode Checkout with
// invoice_creation sends checkout.session.completed and invoice.paid for
// the same money. The in-process lock and the link lookup used to be the
// only guard: a failed link write, or a second process, credited twice.
// The credit now claims the payment in its own transaction.

// staleLookupStore answers every commission lookup with "not found", like
// a second process that looked before the first one committed. Only the
// engine's payment claim can stop the second credit.
type staleLookupStore struct{ *billing.SQLStore }

func (staleLookupStore) FindCommission(context.Context, string, ...string) (int64, bool, error) {
	return 0, false, nil
}

func manualInvoice(pi string, withPI bool) map[string]any {
	o := map[string]any{
		"id": "in_" + pi, "amount_paid": 10000, "total_excluding_tax": 10000, "currency": "usd",
		"customer": "cus_m", "billing_reason": "manual", "created": 1700000001,
	}
	if withPI {
		o["payment_intent"] = pi
	}
	return o
}

func TestStripe_SinglePaymentCredit(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		invoiceFirst bool
		invoiceHasPI bool
	}{
		{"checkout first", false, true},
		{"invoice first", true, true},
		{"basil invoice, checkout first", false, false},
		{"basil invoice, invoice first", true, false},
	} {
		t.Run(tc.name+", lookup blind", func(t *testing.T) {
			engine, sqlStore, d := newRealMoney(t)
			a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), staleLookupStore{sqlStore})
			co := mfEvent("evt_sc1", "checkout.session.completed", mfCheckout("pi_s1", "in_pi_s1", 10000, 0))
			inv := mfEvent("evt_si1", "invoice.paid", manualInvoice("pi_s1", tc.invoiceHasPI))
			first, second := co, inv
			if tc.invoiceFirst {
				first, second = inv, co
			}
			stripeSend(t, a, first)
			stripeSend(t, a, second)
			stripeSend(t, a, second) // replay
			if n := commissionCount(t, d); n != 1 {
				t.Fatalf("commissions=%d want 1", n)
			}
		})
		t.Run(tc.name+", link write fails", func(t *testing.T) {
			engine, sqlStore, d := newRealMoney(t)
			a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), &flakyLinkStore{SQLStore: sqlStore})
			co := mfEvent("evt_sc2", "checkout.session.completed", mfCheckout("pi_s2", "in_pi_s2", 10000, 0))
			inv := mfEvent("evt_si2", "invoice.paid", manualInvoice("pi_s2", tc.invoiceHasPI))
			first, second := co, inv
			if tc.invoiceFirst {
				first, second = inv, co
			}
			// The first event credits, then its link write fails: the
			// webhook errors and Stripe will retry it.
			if err := a.HandleWebhook(ctx, first, signedHeaders(first)); err == nil {
				t.Fatal("link failure was ACKed")
			}
			stripeSend(t, a, second)
			if n := commissionCount(t, d); n != 1 {
				t.Fatalf("commissions=%d want 1", n)
			}
			if tc.invoiceFirst && !tc.invoiceHasPI {
				// Only the invoice credited, and it names no pi_: the
				// refund reaches it through invoice_payment.paid.
				stripeSend(t, a, mfEvent("evt_sp2", "invoice_payment.paid", map[string]any{
					"id": "inpay_s2", "invoice": "in_pi_s2", "status": "paid",
					"payment": map[string]any{"type": "payment_intent", "payment_intent": "pi_s2"},
				}))
			}
			// A refund before Stripe's retry claws that one commission,
			// found through the payment claim.
			stripeSend(t, a, mfEvent("evt_sr2", "charge.refunded", stripeCharge("pi_s2", 10000, 10000)))
			if _, claw := clawbackFor(t, d); claw != -1000 {
				t.Fatalf("clawback=%d want -1000", claw)
			}
			stripeSend(t, a, first) // Stripe's retry
			if n := commissionCount(t, d); n != 1 {
				t.Fatalf("after retry: commissions=%d want 1", n)
			}
			if _, claw := clawbackFor(t, d); claw != -1000 {
				t.Fatalf("after retry: clawback=%d want -1000", claw)
			}
		})
	}
}

// Different payments of one customer are not merged by the claim.
func TestStripe_SinglePaymentCreditKeepsOtherPayments(t *testing.T) {
	engine, sqlStore, d := newRealMoney(t)
	a := billing.NewStripeAdapter(testSecret, engine, mockResolving(1, 1), staleLookupStore{sqlStore})
	stripeSend(t, a, mfEvent("evt_so1", "checkout.session.completed", mfCheckout("pi_so1", "", 10000, 0)))
	stripeSend(t, a, mfEvent("evt_so2", "checkout.session.completed", mfCheckout("pi_so2", "", 10000, 0)))
	if n := commissionCount(t, d); n != 2 {
		t.Fatalf("commissions=%d want 2", n)
	}
}
