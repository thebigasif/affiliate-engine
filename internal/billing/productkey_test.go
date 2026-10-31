package billing_test

import (
	"context"
	"testing"
	"time"
)

// FR-12 per-product rates: every adapter extracts a ProductKey on purchase
// events. Precedence: explicit metadata pin (aff_product > product) wins;
// fallback is the first line item's price id (the plan purchased).

// --- Stripe ---

// The checkout fixture gains line_items; metadata pin (aff_product) wins.
func TestStripe_ProductKey_MetadataPinWins(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := []byte(`{
	  "id": "evt_pk_1",
	  "type": "checkout.session.completed",
	  "data": {"object": {
	    "id": "cs_pk_1",
	    "object": "checkout.session",
	    "mode": "payment",
	    "payment_status": "paid",
	    "amount_total": 10000,
	    "currency": "usd",
	    "customer": "cus_1",
	    "payment_intent": "pi_pk_1",
	    "created": 1700000000,
	    "metadata": {"aff_product": "pro_monthly"},
	    "line_items": [{"price": {"id": "price_999"}}]
	  }}
	}`)
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("purchases=%d", len(engine.purchases))
	}
	if got := engine.purchases[0].ev.ProductKey; got != "pro_monthly" {
		t.Fatalf("ProductKey=%q want metadata pin pro_monthly", got)
	}
}

// No metadata pin: the first line item's price id is the key.
func TestStripe_ProductKey_LineItemFallback(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := []byte(`{
	  "id": "evt_pk_2",
	  "type": "invoice.paid",
	  "data": {"object": {
	    "id": "in_pk_2",
	    "amount_paid": 9000,
	    "currency": "usd",
	    "customer": "cus_2",
	    "billing_reason": "subscription_create",
	    "created": 1700000150,
	    "lines": {"data": [{"price": {"id": "price_basic_mo"}}]}
	  }}
	}`)
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
	if got := engine.purchases[0].ev.ProductKey; got != "price_basic_mo" {
		t.Fatalf("ProductKey=%q want price_basic_mo (line fallback)", got)
	}
}

// "product" alias also honored; empty pins fall through.
func TestStripe_ProductKey_AliasAndEmpty(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := []byte(`{
	  "id": "evt_pk_3",
	  "type": "checkout.session.completed",
	  "data": {"object": {
	    "id": "cs_pk_3",
	    "mode": "payment",
	    "payment_status": "paid",
	    "amount_total": 10000,
	    "currency": "usd",
	    "customer": "cus_3",
	    "payment_intent": "pi_pk_3",
	    "created": 1700000000,
	    "metadata": {"product": "biz_annual"},
	    "line_items": [{"price": {"id": "price_999"}}]
	  }}
	}`)
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
	if got := engine.purchases[0].ev.ProductKey; got != "biz_annual" {
		t.Fatalf("ProductKey=%q want alias biz_annual", got)
	}
}

// --- Paddle ---

// custom_data pin (aff_product) > product > first item price id.
func TestPaddle_ProductKey(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	// pin wins
	body := paddleEnvelope("transaction.completed", "evt_pk_p1", map[string]any{
		"id": "txn_p1", "currency_code": "USD",
		"totals":      map[string]string{"grand_total": "100"},
		"custom_data": map[string]string{"aff_product": "pro_m"},
		"items":       []any{map[string]any{"price": map[string]any{"id": "pri_999"}}},
	})
	if err := a.HandleWebhook(context.Background(), body,
		paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), body))); err != nil {
		t.Fatal(err)
	}
	if got := mock.purchases[0].ProductKey; got != "pro_m" {
		t.Fatalf("ProductKey=%q want pin pro_m", got)
	}
	// alias
	body2 := paddleEnvelope("transaction.completed", "evt_pk_p2", map[string]any{
		"id": "txn_p2", "currency_code": "USD",
		"totals":      map[string]string{"grand_total": "100"},
		"custom_data": map[string]string{"product": "biz_yr"},
		"items":       []any{map[string]any{"price": map[string]any{"id": "pri_999"}}},
	})
	if err := a.HandleWebhook(context.Background(), body2,
		paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), body2))); err != nil {
		t.Fatal(err)
	}
	if got := mock.purchases[1].ProductKey; got != "biz_yr" {
		t.Fatalf("ProductKey=%q want alias biz_yr", got)
	}
	// line-item fallback
	body3 := paddleEnvelope("transaction.completed", "evt_pk_p3", map[string]any{
		"id": "txn_p3", "currency_code": "USD",
		"totals": map[string]string{"grand_total": "100"},
		"items":  []any{map[string]any{"price": map[string]any{"id": "pri_456"}}},
	})
	if err := a.HandleWebhook(context.Background(), body3,
		paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), body3))); err != nil {
		t.Fatal(err)
	}
	if got := mock.purchases[2].ProductKey; got != "pri_456" {
		t.Fatalf("ProductKey=%q want line item pri_456", got)
	}
}

// --- Chargebee ---

// First line item entity_id is the product key; none -> "".
func TestChargebee_ProductKey(t *testing.T) {
	a, mock, _ := newCBAdapter()
	with := cbEnvelope("invoice.paid", "evt_pk_cb1", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "in_cb1", "total": 100, "currency_code": "USD",
			"line_items": []any{map[string]any{"entity_id": "cb_pro_mo"}, map[string]any{"entity_id": "cb_addon"}},
		},
	})
	if err := a.HandleWebhook(context.Background(), with, cbHeaders(signCB("cb_test_secret", with))); err != nil {
		t.Fatal(err)
	}
	if got := mock.purchases[0].ProductKey; got != "cb_pro_mo" {
		t.Fatalf("ProductKey=%q want first line entity_id cb_pro_mo", got)
	}
	// No lines: empty key (product-agnostic resolution).
	without := cbEnvelope("invoice.paid", "evt_pk_cb2", map[string]any{
		"invoice": map[string]any{
			"invoice_id": "in_cb2", "total": 100, "currency_code": "USD",
		},
	})
	if err := a.HandleWebhook(context.Background(), without, cbHeaders(signCB("cb_test_secret", without))); err != nil {
		t.Fatal(err)
	}
	if got := mock.purchases[1].ProductKey; got != "" {
		t.Fatalf("ProductKey=%q want empty", got)
	}
}
