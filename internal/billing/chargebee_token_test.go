package billing_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
)

func cbSend(t *testing.T, a *billing.ChargebeeAdapter, eventType, id string, content map[string]any) {
	t.Helper()
	body := cbEnvelope(eventType, id, content)
	if err := a.HandleWebhook(context.Background(), body, cbHeaders(signCB("cb_test_secret", body))); err != nil {
		t.Fatalf("%s %s: %v", eventType, id, err)
	}
}

type cbRecordingResolver struct{ queries []attribution.Query }

func (r *cbRecordingResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	r.queries = append(r.queries, q)
	return attribution.Resolution{}, nil
}

// The customer's cf_aff_token reaches the attribution ladder, raw or in
// the at1_ client-reference form.
func TestChargebee_CfAffTokenReachesResolver(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"payload.sig", "payload.sig"},
		{"at1_payload_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", "payload.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"},
	} {
		r := &cbRecordingResolver{}
		a := billing.NewChargebeeAdapter("cb_test_secret", &paddleMock{}, r, newStubStore())
		cbSend(t, a, "payment_succeeded", "ev_tok", map[string]any{
			"invoice":  map[string]any{"id": "inv_t", "customer_id": "c_t", "status": "paid", "currency_code": "USD", "total": 4900},
			"customer": map[string]any{"id": "c_t", "cf_aff_token": tc.in},
		})
		if len(r.queries) != 1 || r.queries[0].Token != tc.want || r.queries[0].CustomerExtID != "c_t" {
			t.Fatalf("cf_aff_token %q: queries=%+v", tc.in, r.queries)
		}
	}
}

// Real engine: a trial signup binds the customer, the first paid invoice
// credits through the binding, a refund sent as both credit_note_created
// and payment_refunded claws once, and an adjustment credit note claws
// nothing. A first sale with the token credits on the token rung.
func TestChargebee_TrialBindRefundOnceRealMoney(t *testing.T) {
	eng, store, db := newRealMoney(t)
	a := billing.NewChargebeeAdapter("cb_test_secret", eng, tokenResolver{}, store)

	cbSend(t, a, "subscription_created", "ev_sub", map[string]any{
		"subscription": map[string]any{"id": "sub_50", "status": "in_trial"},
		"customer":     map[string]any{"id": "c50", "cf_aff_token": "payload.sig"},
	})
	cbSend(t, a, "payment_succeeded", "ev_zero", map[string]any{
		"invoice":  map[string]any{"id": "inv_0", "customer_id": "c50", "status": "paid", "currency_code": "USD", "total": 0},
		"customer": map[string]any{"id": "c50"},
	})
	var bound int64
	if err := db.QueryRow(`SELECT affiliate_id FROM customers WHERE external_customer_id = 'c50'`).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("trial did not bind: %d %v", bound, err)
	}
	cbSend(t, a, "payment_succeeded", "ev_paid", map[string]any{
		"invoice":  map[string]any{"id": "inv_50", "customer_id": "c50", "status": "paid", "currency_code": "USD", "total": 5000},
		"customer": map[string]any{"id": "c50"},
	})
	var amount int64
	var method string
	if err := db.QueryRow(`SELECT amount, attribution_method FROM commissions WHERE parent_commission_id IS NULL`).Scan(&amount, &method); err != nil {
		t.Fatal(err)
	}
	if amount != 500 || method != "customer" {
		t.Fatalf("first paid invoice: amount=%d method=%s", amount, method)
	}

	cn := map[string]any{"id": "cn_50", "reference_invoice_id": "inv_50", "type": "refundable", "currency_code": "USD", "total": 1250}
	cbSend(t, a, "credit_note_created", "ev_cn", map[string]any{"credit_note": cn})
	cbSend(t, a, "payment_refunded", "ev_ref", map[string]any{
		"credit_note": cn,
		"transaction": map[string]any{"id": "txn_r", "amount": 1250, "linked_invoices": []any{map[string]any{"invoice_id": "inv_50"}}},
	})
	if _, claw := clawbackFor(t, db); claw != -125 {
		t.Fatalf("clawback=%d want -125 once (25%% of 500)", claw)
	}
	cbSend(t, a, "credit_note_created", "ev_adj", map[string]any{"credit_note": map[string]any{
		"id": "cn_adj", "reference_invoice_id": "inv_50", "type": "adjustment", "currency_code": "USD", "total": 1000,
	}})
	if _, claw := clawbackFor(t, db); claw != -125 {
		t.Fatalf("adjustment credit note clawed: %d", claw)
	}

	// A first sale with the token on the customer credits on the token rung.
	cbSend(t, a, "payment_succeeded", "ev_first", map[string]any{
		"invoice":  map[string]any{"id": "inv_51", "customer_id": "c51", "status": "paid", "currency_code": "USD", "total": 2000},
		"customer": map[string]any{"id": "c51", "cf_aff_token": "payload.sig"},
	})
	if err := db.QueryRow(`SELECT attribution_method FROM commissions WHERE customer_ext_id = 'c51'`).Scan(&method); err != nil || method != "token" {
		t.Fatalf("tokened first sale: method=%q err=%v", method, err)
	}
}
