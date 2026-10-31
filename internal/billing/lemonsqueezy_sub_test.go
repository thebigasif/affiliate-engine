package billing_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// lsEnvelopeCustom builds a live envelope with meta.custom_data, where
// Lemon Squeezy really puts checkout[custom][...] values.
func lsEnvelopeCustom(eventName, dataType, id string, attrs, custom map[string]any) []byte {
	meta := map[string]any{"event_name": eventName, "test_mode": false}
	if custom != nil {
		meta["custom_data"] = custom
	}
	b, _ := json.Marshal(map[string]any{
		"meta": meta,
		"data": map[string]any{"id": id, "type": dataType, "attributes": attrs},
	})
	return b
}

func lsInvoiceAttrs(reason string, total, refunded int64) map[string]any {
	return map[string]any{
		"subscription_id": 501, "customer_id": 77, "user_email": "buyer@example.com",
		"billing_reason": reason, "currency": "usd", "status": "paid",
		"total": total, "refunded": refunded > 0, "refunded_amount": refunded,
	}
}

func lsSend(t *testing.T, a *billing.LemonSqueezyAdapter, body []byte) {
	t.Helper()
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
}

// tokenResolver resolves any non-empty token to affiliate 1 ("token"
// rung) and nothing else, so the engine falls back to its own ladder.
type tokenResolver struct{}

func (tokenResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	if q.Token == "" {
		return attribution.Resolution{}, nil
	}
	return attribution.Resolution{AffiliateID: 1, Method: "token", CampaignID: 1}, nil
}

// The checkout token arrives in meta.custom_data, not in the order's
// attributes; it must reach the attribution ladder.
func TestLemonSqueezy_MetaCustomDataToken(t *testing.T) {
	a, mock, _ := newLSAdapter()
	lsSend(t, a, lsEnvelopeCustom("order_created", "orders", "2001",
		lsOrderAttrs("2001", 5000, 0, nil), map[string]any{"token": "tok_meta", "coupon": "LAUNCH"}))
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	if p := mock.purchases[0]; p.ClickToken != "tok_meta" || p.CouponCode != "LAUNCH" {
		t.Fatalf("meta.custom_data not read: %+v", p)
	}
}

// Renewals credit; the "initial" invoice (the order's own payment) and
// unpaid invoices do not.
func TestLemonSqueezy_SubscriptionPayments(t *testing.T) {
	a, mock, store := newLSAdapter()
	lsSend(t, a, lsEnvelopeCustom("subscription_payment_success", "subscription-invoices", "inv-0", lsInvoiceAttrs("initial", 2000, 0), nil))
	unpaid := lsInvoiceAttrs("renewal", 2000, 0)
	unpaid["status"] = "pending"
	lsSend(t, a, lsEnvelopeCustom("subscription_payment_success", "subscription-invoices", "inv-p", unpaid, nil))
	if len(mock.purchases) != 0 {
		t.Fatalf("initial/unpaid invoice credited: %+v", mock.purchases)
	}
	lsSend(t, a, lsEnvelopeCustom("subscription_payment_success", "subscription-invoices", "inv-1", lsInvoiceAttrs("renewal", 2000, 0), nil))
	if len(mock.purchases) != 1 {
		t.Fatalf("renewal purchases=%d", len(mock.purchases))
	}
	p := mock.purchases[0]
	if p.ExternalID != "subscription_payment_success:inv-1" || p.AmountMinor != 2000 ||
		p.CustomerExtID != "77" || p.Currency != "USD" || p.EventType != "purchase" {
		t.Fatalf("renewal event=%+v", p)
	}
	if store.links["lemonsqueezy:subinv:inv-1"] == 0 {
		t.Fatal("invoice not linked for refunds")
	}

	lsSend(t, a, lsEnvelopeCustom("subscription_payment_refunded", "subscription-invoices", "inv-1", lsInvoiceAttrs("renewal", 2000, 500), nil))
	if len(mock.refunds) != 1 || mock.refunds[0].refunded != 500 || mock.refunds[0].original != 2000 {
		t.Fatalf("renewal refund=%+v", mock.refunds)
	}
	if mock.refunds[0].ev.ExternalID != "subscription_payment_refunded:inv-1:500" {
		t.Fatalf("refund key=%q", mock.refunds[0].ev.ExternalID)
	}
	// An initial invoice refund is the order's, clawed by order_refunded.
	lsSend(t, a, lsEnvelopeCustom("subscription_payment_refunded", "subscription-invoices", "inv-0", lsInvoiceAttrs("initial", 2000, 2000), nil))
	if len(mock.refunds) != 1 || len(store.misses) != 0 {
		t.Fatalf("initial invoice refund handled: refunds=%d misses=%v", len(mock.refunds), store.misses)
	}
}

// End to end on the real engine: a free-trial checkout ($0 order) binds
// the customer, the first paid renewal credits through the binding, and
// a partial refund of that renewal claws its share.
func TestLemonSqueezy_TrialRenewalRealMoney(t *testing.T) {
	eng, store, db := newRealMoney(t)
	a := billing.NewLemonSqueezyAdapter("ls_test_secret", eng, tokenResolver{}, store)

	lsSend(t, a, lsEnvelopeCustom("order_created", "orders", "3001",
		lsOrderAttrs("3001", 0, 0, nil), map[string]any{"token": "tok_trial"}))
	lsSend(t, a, lsEnvelopeCustom("subscription_created", "subscriptions", "501",
		map[string]any{"customer_id": 77, "user_email": "buyer@example.com", "variant_id": 42},
		map[string]any{"token": "tok_trial"}))
	var bound int64
	if err := db.QueryRow(`SELECT affiliate_id FROM customers WHERE external_customer_id = '77'`).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("trial checkout did not bind: affiliate=%d err=%v", bound, err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	if n != 0 {
		t.Fatalf("$0 trial order credited %d commissions", n)
	}

	lsSend(t, a, lsEnvelopeCustom("subscription_payment_success", "subscription-invoices", "inv-9",
		lsInvoiceAttrs("renewal", 2000, 0), nil))
	var amount int64
	var method string
	if err := db.QueryRow(`SELECT amount, attribution_method FROM commissions WHERE parent_commission_id IS NULL`).Scan(&amount, &method); err != nil {
		t.Fatal(err)
	}
	if amount != 200 || method != "customer" {
		t.Fatalf("renewal commission amount=%d method=%s; want 200 via the binding", amount, method)
	}
	// Replay: no second commission.
	lsSend(t, a, lsEnvelopeCustom("subscription_payment_success", "subscription-invoices", "inv-9",
		lsInvoiceAttrs("renewal", 2000, 0), nil))
	db.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("replayed renewal: %d commissions", n)
	}

	lsSend(t, a, lsEnvelopeCustom("subscription_payment_refunded", "subscription-invoices", "inv-9",
		lsInvoiceAttrs("renewal", 2000, 500), nil))
	if _, claw := clawbackFor(t, db); claw != -50 {
		t.Fatalf("clawback=%d want -50 (25%% of 200)", claw)
	}
}
