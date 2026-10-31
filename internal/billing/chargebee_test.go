package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

func signCB(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func cbEnvelope(eventType, id string, content any) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": id, "event_type": eventType, "content": content,
	})
	return b
}

func cbHeaders(sig string) http.Header {
	h := http.Header{}
	h.Set("cb-signature", sig)
	return h
}

// cbBasicHeaders is Chargebee's native webhook auth: HTTP Basic.
func cbBasicHeaders(user, pass string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	return h
}

func newCBAdapter() (*billing.ChargebeeAdapter, *paddleMock, *stubStore) {
	m := &paddleMock{}
	st := newStubStore()
	return billing.NewChargebeeAdapter("cb_test_secret", m, nopResolver{}, st), m, st
}

func newCBBasicAdapter() (*billing.ChargebeeAdapter, *paddleMock, *stubStore) {
	m := &paddleMock{}
	st := newStubStore()
	return billing.NewChargebeeBasicAuthAdapter("cb_user", "cb_pass", m, nopResolver{}, st), m, st
}

// CB1: invoice paid -> purchase + invoice-id link (refund path depends on it).
func TestChargebee_InvoicePaid(t *testing.T) {
	a, mock, store := newCBAdapter()
	body := cbEnvelope("invoice.paid", "evt_cb1", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "inv_1", "customer_id": "cb_cus_1",
			"currency_code": "USD", "total": 6600,
		},
	})
	err := a.HandleWebhook(context.Background(), body, cbHeaders(signCB("cb_test_secret", body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	p := mock.purchases[0]
	if p.Source != "chargebee" || p.AmountMinor != 6600 || p.CustomerExtID != "cb_cus_1" {
		t.Fatalf("event=%+v", p)
	}
	if store.links["chargebee:inv_1"] == 0 {
		t.Fatal("invoice id not linked")
	}
}

// CB2: signature enforcement (missing/wrong/forged).
func TestChargebee_Signature(t *testing.T) {
	a, mock, _ := newCBAdapter()
	body := cbEnvelope("invoice.paid", "evt_cb2", map[string]any{
		"invoice": map[string]any{"invoice_id": "i", "total": 100, "currency_code": "USD"},
	})
	if err := a.HandleWebhook(context.Background(), body, http.Header{}); err == nil {
		t.Fatal("missing sig accepted")
	}
	if err := a.HandleWebhook(context.Background(), body, cbHeaders("bogus==")); err == nil {
		t.Fatal("bogus sig accepted")
	}
	if err := a.HandleWebhook(context.Background(), body, cbHeaders(signCB("OTHER", body))); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if len(mock.purchases) != 0 {
		t.Fatal("engine called on bad signature")
	}
}

// CB3: credit note refund -> ProcessRefund via invoice link.
func TestChargebee_Refund(t *testing.T) {
	a, mock, store := newCBAdapter()
	// Purchase first.
	inv := cbEnvelope("invoice.paid", "evt_cb3", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "inv_3", "customer_id": "c3", "currency_code": "USD", "total": 10000,
		},
	})
	a.HandleWebhook(context.Background(), inv, cbHeaders(signCB("cb_test_secret", inv)))

	// Refund 30%.
	refund := cbEnvelope("payment_refunded", "evt_cb4", map[string]any{
		"credit_note": map[string]any{
			"cn_id": "cn_1", "invoice_id": "inv_3", "total": -3000,
		},
	})
	if err := a.HandleWebhook(context.Background(), refund, cbHeaders(signCB("cb_test_secret", refund))); err != nil {
		t.Fatal(err)
	}
	if len(mock.refunds) != 1 {
		t.Fatalf("refunds=%d misses=%v", len(mock.refunds), store.misses)
	}
	r := mock.refunds[0]
	if r.refunded != 3000 || r.parent != 1 {
		t.Fatalf("refund: %+v", r)
	}
}

// CB4: unknown events + zero totals silent.
func TestChargebee_SilentSkips(t *testing.T) {
	a, mock, _ := newCBAdapter()
	for _, et := range []string{"subscription.created", "customer.updated", "payment.failed"} {
		body := cbEnvelope(et, "evt_"+et, map[string]any{})
		if err := a.HandleWebhook(context.Background(), body, cbHeaders(signCB("cb_test_secret", body))); err != nil {
			t.Fatalf("%s: %v", et, err)
		}
	}
	zero := cbEnvelope("invoice.paid", "evt_zero", map[string]any{
		"invoice": map[string]any{"invoice_id": "z", "total": 0, "currency_code": "USD"},
	})
	if err := a.HandleWebhook(context.Background(), zero, cbHeaders(signCB("cb_test_secret", zero))); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 0 {
		t.Fatal("zero/unknown commissioned")
	}
	_ = fmt.Sprint
}

// CB5: Basic Auth (Chargebee's native webhook protection) — valid
// credentials pass; missing, wrong password, wrong user, and a valid
// cb-signature WITHOUT Basic Auth are all rejected before the engine.
func TestChargebee_BasicAuth(t *testing.T) {
	a, mock, _ := newCBBasicAdapter()
	body := cbEnvelope("invoice.paid", "evt_cb5", map[string]any{
		"invoice": map[string]any{"invoice_id": "inv_5", "total": 100, "currency_code": "USD"},
	})
	for name, h := range map[string]http.Header{
		"missing":        {},
		"wrong-password": cbBasicHeaders("cb_user", "WRONG"),
		"wrong-user":     cbBasicHeaders("intruder", "cb_pass"),
		"hmac-only":      cbHeaders(signCB("cb_test_secret", body)),
	} {
		if err := a.HandleWebhook(context.Background(), body, h); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if len(mock.purchases) != 0 {
		t.Fatal("engine called on auth failure")
	}
	if err := a.HandleWebhook(context.Background(), body, cbBasicHeaders("cb_user", "cb_pass")); err != nil {
		t.Fatalf("valid basic auth rejected: %v", err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
}

// CB6: an adapter with neither Basic Auth nor a secret fails closed.
func TestChargebee_NoAuthConfiguredRejects(t *testing.T) {
	m := &paddleMock{}
	a := billing.NewChargebeeAdapter("", m, nopResolver{}, newStubStore())
	body := cbEnvelope("invoice.paid", "evt_cb6", map[string]any{
		"invoice": map[string]any{"invoice_id": "inv_6", "total": 100, "currency_code": "USD"},
	})
	if err := a.HandleWebhook(context.Background(), body, cbHeaders(signCB("", body))); err == nil {
		t.Fatal("unconfigured adapter accepted a delivery")
	}
	if len(m.purchases) != 0 {
		t.Fatal("engine called")
	}
}

// CB7: buyer email is read from the nested content.customer.email, with
// invoice.billing_address.email as the fallback (the old dotted json tag
// never matched, so self-referral/fingerprint checks saw no email).
func TestChargebee_EmailExtraction(t *testing.T) {
	a, mock, _ := newCBAdapter()
	both := cbEnvelope("invoice.paid", "evt_cb7a", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "inv_7a", "customer_id": "c7", "total": 100, "currency_code": "USD",
			"billing_address": map[string]any{"email": "billing@example.com", "country": "gb"},
		},
		"customer": map[string]any{"id": "c7", "email": "buyer@example.com"},
	})
	billingOnly := cbEnvelope("invoice.paid", "evt_cb7b", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "inv_7b", "customer_id": "c7", "total": 100, "currency_code": "USD",
			"billing_address": map[string]any{"email": "billing@example.com"},
		},
	})
	for _, b := range [][]byte{both, billingOnly} {
		if err := a.HandleWebhook(context.Background(), b, cbHeaders(signCB("cb_test_secret", b))); err != nil {
			t.Fatal(err)
		}
	}
	if len(mock.purchases) != 2 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	if got := mock.purchases[0].CustomerEmail; got != "buyer@example.com" {
		t.Fatalf("CustomerEmail=%q want content.customer.email", got)
	}
	if got := mock.purchases[0].Country; got != "GB" {
		t.Fatalf("Country=%q want GB from billing_address", got)
	}
	if got := mock.purchases[1].CustomerEmail; got != "billing@example.com" {
		t.Fatalf("CustomerEmail=%q want billing_address.email fallback", got)
	}
}

// CB8: refund proration base is the SALE gross, not the commission amount.
// Real engine: 10% of 10000 = 1000; a 3000 (30%) credit note claws 300.
func TestChargebee_RefundProratesAgainstGross(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewChargebeeAdapter("cb_test_secret", engine, mockResolving(1, 1), store)
	inv := cbEnvelope("invoice.paid", "evt_cb8a", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "inv_8", "customer_id": "c8", "currency_code": "USD", "total": 10000,
		},
	})
	if err := a.HandleWebhook(context.Background(), inv, cbHeaders(signCB("cb_test_secret", inv))); err != nil {
		t.Fatal(err)
	}
	refund := cbEnvelope("payment_refunded", "evt_cb8b", map[string]any{
		"credit_note": map[string]any{
			"cn_id": "cn_8", "invoice_id": "inv_8", "currency_code": "USD", "total": -3000,
		},
	})
	if err := a.HandleWebhook(context.Background(), refund, cbHeaders(signCB("cb_test_secret", refund))); err != nil {
		t.Fatal(err)
	}
	if _, claw := clawbackFor(t, d); claw != -300 {
		t.Fatalf("clawback=%d want -300 (30%% of the 1000 commission)", claw)
	}
}

// CB9: real Chargebee payload shapes — invoice.status is a STRING (the old
// struct type failed every real invoice with a decode error), the invoice
// id arrives as "id", and a credit note references its invoice via
// reference_invoice_id. Unpaid invoices do not credit.
func TestChargebee_RealPayloadShapes(t *testing.T) {
	a, mock, store := newCBAdapter()
	paid := cbEnvelope("payment_succeeded", "evt_cb9a", map[string]any{
		"invoice": map[string]any{
			"id": "inv_9", "customer_id": "c9", "status": "paid",
			"currency_code": "USD", "total": 10000,
		},
		"customer": map[string]any{"id": "c9", "email": "buyer9@example.com"},
	})
	if err := a.HandleWebhook(context.Background(), paid, cbHeaders(signCB("cb_test_secret", paid))); err != nil {
		t.Fatalf("real-shape paid invoice rejected: %v", err)
	}
	if len(mock.purchases) != 1 || mock.purchases[0].OrderID != "inv_9" {
		t.Fatalf("purchases=%+v", mock.purchases)
	}
	if store.links["chargebee:inv_9"] == 0 {
		t.Fatal("invoice id (from \"id\") not linked")
	}
	unpaid := cbEnvelope("payment_succeeded", "evt_cb9b", map[string]any{
		"invoice": map[string]any{
			"id": "inv_9b", "customer_id": "c9", "status": "payment_due",
			"currency_code": "USD", "total": 10000,
		},
	})
	if err := a.HandleWebhook(context.Background(), unpaid, cbHeaders(signCB("cb_test_secret", unpaid))); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatal("unpaid invoice credited")
	}
	refund := cbEnvelope("payment_refunded", "evt_cb9c", map[string]any{
		"credit_note": map[string]any{
			"id": "cn_9", "reference_invoice_id": "inv_9", "currency_code": "USD", "total": 2500,
		},
	})
	if err := a.HandleWebhook(context.Background(), refund, cbHeaders(signCB("cb_test_secret", refund))); err != nil {
		t.Fatal(err)
	}
	if len(mock.refunds) != 1 || mock.refunds[0].refunded != 2500 || mock.refunds[0].parent != 1 {
		t.Fatalf("refunds=%+v misses=%v", mock.refunds, store.misses)
	}
}
