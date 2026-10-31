package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// --- Lemon Squeezy fixtures (docs: X-Signature = hex HMAC-SHA256(secret, rawBody)) ---

func signLS(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// lsEnvelope builds a LIVE-mode envelope (test_mode=false).
func lsEnvelope(eventName string, order map[string]any) []byte {
	return lsEnvelopeMode(eventName, false, order)
}

// lsEnvelopeMode builds an envelope with an explicit meta.test_mode.
func lsEnvelopeMode(eventName string, testMode bool, order map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"meta": map[string]any{"event_name": eventName, "test_mode": testMode},
		"data": map[string]any{
			"id":         order["id"],
			"type":       "orders",
			"attributes": order["attributes"],
		},
	})
	return b
}

// lsOrderAttrs builds an order-attributes map. total/refundedAmount are
// INTEGER minor units (cents) — the LS API convention, same as Stripe/
// Chargebee. refundedAmount 0 = never refunded (refunded=false).
func lsOrderAttrs(id string, total, refundedAmount int64, custom map[string]any) map[string]any {
	attrs := map[string]any{
		"identifier":      "ord-uuid-" + id,
		"order_number":    1000 + len(id),
		"user_email":      "buyer@example.com",
		"customer_id":     77,
		"currency":        "USD",
		"subtotal":        total,
		"total":           total,
		"status":          "paid",
		"refunded":        refundedAmount > 0,
		"refunded_amount": refundedAmount,
	}
	if custom != nil {
		attrs["custom_data"] = custom
	}
	attrs["first_order_item"] = map[string]any{"product_id": 9, "variant_id": 42}
	return attrs
}

func lsHeaders(sig string) http.Header {
	h := http.Header{}
	h.Set("X-Signature", sig)
	return h
}

func newLSAdapter() (*billing.LemonSqueezyAdapter, *paddleMock, *stubStore) {
	m := &paddleMock{}
	st := newStubStore()
	return billing.NewLemonSqueezyAdapter("ls_test_secret", m, nopResolver{}, st), m, st
}

// lsRecordingResolver records the attribution.Query it was handed.
type lsRecordingResolver struct {
	queries []attribution.Query
}

func (r *lsRecordingResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	r.queries = append(r.queries, q)
	return attribution.Resolution{}, nil
}

// LS1: order_created (paid, total>0) -> purchase with integer-cents amount
// as-is, variant product key, customer binding ids, and links to both the
// event id and the order id (refund path depends on both).
func TestLemonSqueezy_OrderCreated(t *testing.T) {
	a, mock, store := newLSAdapter()
	body := lsEnvelope("order_created", map[string]any{
		"id": "1050", "attributes": lsOrderAttrs("1050", 185976, 0,
			map[string]any{"token": "tok_abc", "coupon": "SAVE10"}),
	})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	p := mock.purchases[0]
	if p.Source != "lemonsqueezy" || p.AmountMinor != 185976 {
		t.Fatalf("event=%+v (want minor=185976, the integer total as sent)", p)
	}
	if p.Currency != "USD" || p.CustomerExtID != "77" || p.CustomerEmail != "buyer@example.com" {
		t.Fatalf("event=%+v", p)
	}
	if p.ClickToken != "tok_abc" || p.CouponCode != "SAVE10" {
		t.Fatalf("token/coupon not mapped: %+v", p)
	}
	if p.ProductKey != "42" {
		t.Fatalf("ProductKey=%q want variant 42", p.ProductKey)
	}
	if p.ExternalID != "order_created:1050" {
		t.Fatalf("ExternalID=%q", p.ExternalID)
	}
	if store.links["lemonsqueezy:1050"] == 0 {
		t.Fatal("order id not linked")
	}
	if store.links["lemonsqueezy:ord-uuid-1050"] == 0 {
		t.Fatal("order identifier not linked")
	}
}

// LS2: attribution ladder carries token + coupon (Paddle/Stripe contract).
func TestLemonSqueezy_AttributionQuery(t *testing.T) {
	m := &paddleMock{}
	res := &lsRecordingResolver{}
	a := billing.NewLemonSqueezyAdapter("ls_test_secret", m, res, newStubStore())
	body := lsEnvelope("order_created", map[string]any{
		"id": "1051", "attributes": lsOrderAttrs("1051", 4900, 0,
			map[string]any{"token": "tok_xyz", "coupon": "WELCOME"}),
	})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	if len(res.queries) != 1 {
		t.Fatalf("resolve calls=%d", len(res.queries))
	}
	q := res.queries[0]
	if q.Token != "tok_xyz" {
		t.Fatalf("Token=%q — token rung dead for LS conversions", q.Token)
	}
	if q.CouponCode != "WELCOME" {
		t.Fatalf("CouponCode=%q — coupon rung dead for LS conversions", q.CouponCode)
	}
	if q.CustomerExtID != "77" {
		t.Fatalf("CustomerExtID=%q", q.CustomerExtID)
	}
}

// LS3: signature enforcement (missing / bad hex / wrong secret), and the
// engine is never called on failure.
func TestLemonSqueezy_Signature(t *testing.T) {
	a, mock, _ := newLSAdapter()
	body := lsEnvelope("order_created", map[string]any{
		"id": "1052", "attributes": lsOrderAttrs("1052", 1000, 0, nil),
	})
	if err := a.HandleWebhook(context.Background(), body, http.Header{}); err == nil {
		t.Fatal("missing sig accepted")
	}
	if err := a.HandleWebhook(context.Background(), body, lsHeaders("zz-nothex")); err == nil {
		t.Fatal("bad hex accepted")
	}
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("OTHER", body))); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if len(mock.purchases) != 0 {
		t.Fatal("engine called on bad signature")
	}
}

// LS4: unattributed organic order -> ACK (nil error), not a 400 loop.
func TestLemonSqueezy_UnattributedACK(t *testing.T) {
	m := &paddleMock{}
	m.failNoAttribution = true
	a := billing.NewLemonSqueezyAdapter("ls_test_secret", m, nopResolver{}, newStubStore())
	body := lsEnvelope("order_created", map[string]any{
		"id": "1053", "attributes": lsOrderAttrs("1053", 2500, 0, nil),
	})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatalf("organic sale must ACK, got: %v", err)
	}
}

// LS5: zero-total (100% discount) and unpaid orders skip silently.
func TestLemonSqueezy_SilentSkips(t *testing.T) {
	a, mock, _ := newLSAdapter()
	body := lsEnvelope("order_created", map[string]any{
		"id": "1054", "attributes": lsOrderAttrs("1054", 0, 0, nil),
	})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	attrs := lsOrderAttrs("1055", 2000, 0, nil)
	attrs["status"] = "pending"
	body2 := lsEnvelope("order_created", map[string]any{"id": "1055", "attributes": attrs})
	if err := a.HandleWebhook(context.Background(), body2, lsHeaders(signLS("ls_test_secret", body2))); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"subscription_created", "customer_created"} {
		b := lsEnvelope(ev, map[string]any{"id": "x-" + ev, "attributes": lsOrderAttrs(ev, 1000, 0, nil)})
		if err := a.HandleWebhook(context.Background(), b, lsHeaders(signLS("ls_test_secret", b))); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
	}
	if len(mock.purchases) != 0 {
		t.Fatal("zero/unknown/unpaid commissioned")
	}
}

// LS6: refund -> ProcessRefund with cumulative refunded_amount (integer
// cents, as-is), parent found via order-id/identifier links, proration
// base = the order total.
func TestLemonSqueezy_Refund(t *testing.T) {
	a, mock, store := newLSAdapter()
	purchase := lsEnvelope("order_created", map[string]any{
		"id": "1056", "attributes": lsOrderAttrs("1056", 10000, 0, nil),
	})
	a.HandleWebhook(context.Background(), purchase, lsHeaders(signLS("ls_test_secret", purchase)))

	refund := lsEnvelope("order_refunded", map[string]any{
		"id": "1056", "attributes": lsOrderAttrs("1056", 10000, 3000, nil),
	})
	if err := a.HandleWebhook(context.Background(), refund, lsHeaders(signLS("ls_test_secret", refund))); err != nil {
		t.Fatal(err)
	}
	if len(mock.refunds) != 1 {
		t.Fatalf("refunds=%d misses=%v", len(mock.refunds), store.misses)
	}
	r := mock.refunds[0]
	if r.refunded != 3000 {
		t.Fatalf("refunded=%d want 3000", r.refunded)
	}
	if r.parent != 1 {
		t.Fatalf("parent=%d", r.parent)
	}
	if r.original != 10000 {
		t.Fatalf("original=%d want the purchase total (proration base)", r.original)
	}
}

// LS7: refund with no parent commission -> RecordRefundMiss, no engine call.
func TestLemonSqueezy_RefundMiss(t *testing.T) {
	a, mock, store := newLSAdapter()
	body := lsEnvelope("order_refunded", map[string]any{
		"id": "9999", "attributes": lsOrderAttrs("9999", 5000, 5000, nil),
	})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	if len(store.misses) != 1 {
		t.Fatalf("misses=%v", store.misses)
	}
	if len(mock.refunds) != 0 {
		t.Fatal("engine refund called on miss")
	}
}

// LS8: second partial refund on the same order (cumulative grows) is a
// DISTINCT event id; the engine dedupes replays of the same delivery.
func TestLemonSqueezy_RefundIdempotencyKeys(t *testing.T) {
	a, _, store := newLSAdapter()
	mk := func(refunded int64) []byte {
		return lsEnvelope("order_refunded", map[string]any{
			"id": "1057", "attributes": lsOrderAttrs("1057", 10000, refunded, nil),
		})
	}
	// No parent commission -> RecordRefundMiss path; the miss key embeds
	// the external id, which differs between the two cumulative values.
	first := mk(2500)
	if err := a.HandleWebhook(context.Background(), first, lsHeaders(signLS("ls_test_secret", first))); err != nil {
		t.Fatal(err)
	}
	second := mk(6000)
	if err := a.HandleWebhook(context.Background(), second, lsHeaders(signLS("ls_test_secret", second))); err != nil {
		t.Fatal(err)
	}
	// Replay of the first delivery: same key -> RecordRefundMiss dedupes
	// (ON CONFLICT DO NOTHING); no error either way.
	if err := a.HandleWebhook(context.Background(), first, lsHeaders(signLS("ls_test_secret", first))); err != nil {
		t.Fatal(err)
	}
	if len(store.misses) != 3 || store.misses[0] != "order_refunded:1057:2500" ||
		store.misses[1] != "order_refunded:1057:6000" || store.misses[2] != store.misses[0] {
		t.Fatalf("miss keys=%v", store.misses)
	}
}

// LS9: non-string custom_data values (nested/numeric) never fail the order.
func TestLemonSqueezy_NonStringCustomData(t *testing.T) {
	a, mock, _ := newLSAdapter()
	body := lsEnvelope("order_created", map[string]any{
		"id": "1058", "attributes": lsOrderAttrs("1058", 1500, 0,
			map[string]any{"token": 12345, "coupon": []string{"a"}, "note": "ok"}),
	})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d — non-string custom_data lost the commission", len(mock.purchases))
	}
	if mock.purchases[0].ClickToken != "" || mock.purchases[0].CouponCode != "" {
		t.Fatal("non-string values must map to empty, not error")
	}
}

// LS10: money fields are integer cents already — consumed as-is, never
// multiplied by 100 (the old decimal assumption inflated every sale 100x).
func TestLemonSqueezy_IntegerCents(t *testing.T) {
	a, mock, _ := newLSAdapter()
	for _, cents := range []int64{185976, 4900, 1, 99} {
		id := fmt.Sprintf("106%d", cents)
		body := lsEnvelope("order_created", map[string]any{
			"id": id, "attributes": lsOrderAttrs(id, cents, 0, nil),
		})
		if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
			t.Fatal(err)
		}
		got := mock.purchases[len(mock.purchases)-1].AmountMinor
		if got != cents {
			t.Fatalf("total=%d -> AmountMinor=%d, want %d (no x100 rescale)", cents, got, cents)
		}
	}
}

// LS11: adapter satisfies the normalized Adapter contract.
var _ billing.Adapter = (*billing.LemonSqueezyAdapter)(nil)

// LS12: replayed order_created (same delivery) -> engine dedupes via
// raw_events; adapter returns nil both times.
func TestLemonSqueezy_Replay(t *testing.T) {
	a, mock, _ := newLSAdapter()
	body := lsEnvelope("order_created", map[string]any{"id": "1060", "attributes": lsOrderAttrs("1060", 1234, 0, nil)})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d after replay", len(mock.purchases))
	}
}

// LS13: test-mode deliveries are ACKed without crediting or clawing —
// anyone can mint free test-mode orders.
func TestLemonSqueezy_TestModeSkipped(t *testing.T) {
	a, mock, store := newLSAdapter()
	created := lsEnvelopeMode("order_created", true, map[string]any{
		"id": "1070", "attributes": lsOrderAttrs("1070", 10000, 0, map[string]any{"token": "tok_t"}),
	})
	if err := a.HandleWebhook(context.Background(), created, lsHeaders(signLS("ls_test_secret", created))); err != nil {
		t.Fatalf("test-mode order must ACK, got: %v", err)
	}
	refunded := lsEnvelopeMode("order_refunded", true, map[string]any{
		"id": "1070", "attributes": lsOrderAttrs("1070", 10000, 10000, nil),
	})
	if err := a.HandleWebhook(context.Background(), refunded, lsHeaders(signLS("ls_test_secret", refunded))); err != nil {
		t.Fatalf("test-mode refund must ACK, got: %v", err)
	}
	if len(mock.purchases) != 0 || len(mock.refunds) != 0 || len(store.misses) != 0 {
		t.Fatalf("test-mode touched money: purchases=%d refunds=%d misses=%v",
			len(mock.purchases), len(mock.refunds), store.misses)
	}
}

// LS14: refunded is a boolean flag; the amount is refunded_amount. A
// refunded=true delivery with no refunded_amount claws nothing.
func TestLemonSqueezy_RefundedFlagIsNotAnAmount(t *testing.T) {
	a, mock, _ := newLSAdapter()
	purchase := lsEnvelope("order_created", map[string]any{
		"id": "1071", "attributes": lsOrderAttrs("1071", 10000, 0, nil),
	})
	if err := a.HandleWebhook(context.Background(), purchase, lsHeaders(signLS("ls_test_secret", purchase))); err != nil {
		t.Fatal(err)
	}
	attrs := lsOrderAttrs("1071", 10000, 0, nil)
	attrs["refunded"] = true
	body := lsEnvelope("order_refunded", map[string]any{"id": "1071", "attributes": attrs})
	if err := a.HandleWebhook(context.Background(), body, lsHeaders(signLS("ls_test_secret", body))); err != nil {
		t.Fatalf("boolean refunded must parse, got: %v", err)
	}
	if len(mock.refunds) != 0 {
		t.Fatalf("refund without refunded_amount clawed: %+v", mock.refunds)
	}
}
