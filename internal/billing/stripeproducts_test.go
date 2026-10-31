package billing_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// One Stripe account, several products: AE_STRIPE_PRODUCTS limits which
// sales earn commission. Other products' sales, and their refunds, are
// ignored.

func mustFilter(t *testing.T, list string) billing.ProductFilter {
	t.Helper()
	f, err := billing.ParseProductFilter(list)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseProductFilter(t *testing.T) {
	f := mustFilter(t, "prod_Cloud1, price_Biz2;plink_Buy3\nprod_X")
	if !f.Active() || len(f.IDs) != 4 {
		t.Fatalf("ids = %v", f.IDs)
	}
	for _, bad := range []string{"prod_ok, cus_123", "whsec_abc", "prod_", "prod_a/../b"} {
		if _, err := billing.ParseProductFilter(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if f, err := billing.ParseProductFilter("  "); err != nil || f.Active() {
		t.Fatal("blank list must be inactive")
	}
}

func invoiceWithLine(evt, line string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"type":"invoice.paid","data":{"object":{
	  "id":"in_%s","amount_paid":4900,"total_excluding_tax":4900,"currency":"usd",
	  "customer":"cus_1","payment_intent":"pi_%s","billing_reason":"subscription_create","created":1700000000,
	  "metadata":{"aff_token":"tok"},
	  "lines":{"data":[%s]}}}}`, evt, evt, evt, line))
}

func TestProductFilterInvoices(t *testing.T) {
	cases := []struct {
		name, line string
		counted    bool
	}{
		{"listed product", `{"price":{"id":"price_other","product":"prod_Cloud"}}`, true},
		{"listed price", `{"price":{"id":"price_Biz","product":"prod_nope"}}`, true},
		{"expanded product object", `{"price":{"id":"price_x","product":{"id":"prod_Cloud","name":"Cloud"}}}`, true},
		{"new API shape (pricing.price_details)", `{"pricing":{"price_details":{"price":"price_y","product":"prod_Cloud"}}}`, true},
		{"another product on the account", `{"price":{"id":"price_qev","product":"prod_QEV"}}`, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			engine := &mockEngine{}
			a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
			a.Products = mustFilter(t, "prod_Cloud price_Biz")
			body := invoiceWithLine(fmt.Sprintf("evt_inv_%d", i), c.line)
			if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
				t.Fatal(err)
			}
			if got := len(engine.purchases) == 1; got != c.counted {
				t.Fatalf("counted=%v want %v", got, c.counted)
			}
		})
	}
}

func paymentCheckout(evt, extra string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"type":"checkout.session.completed","data":{"object":{
	  "id":"cs_%s","object":"checkout.session","mode":"payment","payment_status":"paid",
	  "amount_total":9900,"currency":"usd","customer":"cus_2","payment_intent":"pi_%s",
	  "created":1700000000,"metadata":{"aff_token":"tok"}%s}}}`, evt, evt, evt, extra))
}

func TestProductFilterPaymentLink(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	a.Products = mustFilter(t, "plink_SelfHost")
	ours := paymentCheckout("evt_pl_1", `,"payment_link":"plink_SelfHost"`)
	theirs := paymentCheckout("evt_pl_2", `,"payment_link":"plink_QEV"`)
	for _, b := range [][]byte{ours, theirs} {
		if err := a.HandleWebhook(context.Background(), b, signedHeaders(b)); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.purchases) != 1 || engine.purchases[0].ev.ExternalID != "evt_pl_1" {
		t.Fatalf("purchases = %+v, want only the listed payment link", engine.purchases)
	}
}

func TestProductFilterLooksUpLineItems(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer rk_test_key" || r.Header.Get("Stripe-Version") == "" {
			t.Errorf("auth/version headers missing")
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/cs_evt_li_ours/line_items"):
			_, _ = w.Write([]byte(`{"data":[{"price":{"id":"price_1","product":"prod_SelfHost"}}]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/cs_evt_li_theirs/line_items"):
			_, _ = w.Write([]byte(`{"data":[{"price":{"id":"price_2","product":"prod_QEV"}}]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	f := mustFilter(t, "prod_SelfHost")
	f.LineItems = billing.StripeLineItems("rk_test_key", srv.URL)
	a.Products = f

	for _, evt := range []string{"evt_li_ours", "evt_li_theirs"} {
		b := paymentCheckout(evt, "")
		if err := a.HandleWebhook(context.Background(), b, signedHeaders(b)); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.purchases) != 1 || engine.purchases[0].ev.ExternalID != "evt_li_ours" || calls != 2 {
		t.Fatalf("purchases=%d calls=%d", len(engine.purchases), calls)
	}

	// Stripe down: the webhook errors so Stripe retries later. Nothing is
	// counted or dropped for good.
	b := paymentCheckout("evt_li_down", "")
	if err := a.HandleWebhook(context.Background(), b, signedHeaders(b)); err == nil {
		t.Fatal("a failed lookup must make Stripe retry")
	}
	if len(engine.purchases) != 1 {
		t.Fatal("nothing may be counted on a failed lookup")
	}
}

func TestProductFilterFailsClosedWithoutAPIKey(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	a.Products = mustFilter(t, "prod_SelfHost")
	b := paymentCheckout("evt_nokey", "")
	if err := a.HandleWebhook(context.Background(), b, signedHeaders(b)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 0 {
		t.Fatal("a sale we can't identify must not count")
	}
}

func TestProductFilterOffCountsEverything(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	b := invoiceWithLine("evt_off", `{"price":{"id":"price_qev","product":"prod_QEV"}}`)
	if err := a.HandleWebhook(context.Background(), b, signedHeaders(b)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatal("without a filter every sale counts, as before")
	}
}

func TestProductFilterQuietsOtherProductsRefunds(t *testing.T) {
	store := &mockStore{}
	a := newAdapter(&mockEngine{}, mockResolving(7, 2), store)
	a.Products = mustFilter(t, "prod_Cloud")
	b := []byte(`{"id":"evt_rf","type":"charge.refunded","data":{"object":{
	  "id":"ch_qev","amount":2000,"amount_refunded":2000,"currency":"usd","payment_intent":"pi_qev",
	  "refunds":{"data":[{"id":"re_1","amount":2000,"status":"succeeded"}]}}}}`)
	if err := a.HandleWebhook(context.Background(), b, signedHeaders(b)); err != nil {
		t.Fatal(err)
	}
	if len(store.misses) != 0 {
		t.Fatalf("another product's refund recorded as a miss: %+v", store.misses)
	}
}
