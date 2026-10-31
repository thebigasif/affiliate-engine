package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// A Standard Webhooks secret: whsec_ + base64 of the key.
var (
	polarKey    = []byte("polar-test-key-0123456789abcdef")
	polarSecret = "whsec_" + base64.StdEncoding.EncodeToString(polarKey)
)

func signPolar(key []byte, id string, ts int64, body []byte) http.Header {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	h.Set("webhook-signature", "v1,bm90LXRoaXMtb25l v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	return h
}

func polarOrder(typ, id string, net, refunded int64, meta map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "timestamp": "2026-09-30T10:00:00Z", "data": map[string]any{
		"id": id, "status": "paid", "paid": true, "net_amount": net, "total_amount": net + net/10,
		"tax_amount": net / 10, "refunded_amount": refunded, "currency": "usd", "billing_reason": "purchase",
		"customer_id": "cus_1", "product_id": "prod_9", "invoice_number": "INV-1", "metadata": meta,
		"customer": map[string]any{"email": "buyer@example.com"},
	}})
	return b
}

func newPolar() (*billing.PolarAdapter, *paddleMock, *stubStore, *lsRecordingResolver) {
	m, st, r := &paddleMock{}, newStubStore(), &lsRecordingResolver{}
	a := billing.NewPolarAdapter(polarSecret, m, r, st)
	return a, m, st, r
}

// Signed with the Standard Webhooks key or with Polar's earlier key (the
// whole whsec_ string): both verify. Tampered, stale or unsigned: refused.
func TestPolar_Signature(t *testing.T) {
	a, _, _, _ := newPolar()
	ctx := context.Background()
	now := time.Now().Unix()
	body := polarOrder("order.created", "o1", 1000, 0, nil)
	if err := a.HandleWebhook(ctx, body, signPolar(polarKey, "msg_1", now, body)); err != nil {
		t.Fatalf("standard key: %v", err)
	}
	if err := a.HandleWebhook(ctx, body, signPolar([]byte(polarSecret), "msg_2", now, body)); err != nil {
		t.Fatalf("legacy key: %v", err)
	}
	for name, h := range map[string]http.Header{
		"wrong key": signPolar([]byte("nope"), "msg_3", now, body),
		"stale":     signPolar(polarKey, "msg_4", now-600, body),
		"unsigned":  {},
	} {
		if err := a.HandleWebhook(ctx, body, h); !errors.Is(err, billing.ErrInvalidSignature) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	h := signPolar(polarKey, "msg_5", now, body)
	if err := a.HandleWebhook(ctx, append(body, ' '), h); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Fatalf("tampered body: %v", err)
	}
}

// A paid order credits net_amount (before tax) with the reference_id
// token; a replay doesn't credit twice; a $0 order credits nothing; a
// refund claws back the cumulative refunded share.
func TestPolar_PaidAndRefunded(t *testing.T) {
	a, m, st, r := newPolar()
	ctx := context.Background()
	now := time.Now().Unix()
	body := polarOrder("order.paid", "ord_1", 4900, 0, map[string]any{"reference_id": "tok_abc"})
	for i := 0; i < 2; i++ {
		if err := a.HandleWebhook(ctx, body, signPolar(polarKey, "msg_p", now, body)); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.purchases) != 1 {
		t.Fatalf("purchases: %d", len(m.purchases))
	}
	ev := m.purchases[0]
	if ev.Source != "polar" || ev.AmountMinor != 4900 || ev.Currency != "USD" || ev.ProductKey != "prod_9" ||
		ev.CustomerExtID != "cus_1" || ev.OrderID != "INV-1" || ev.ExternalID != "order.paid:ord_1" {
		t.Fatalf("event: %+v", ev)
	}
	if len(r.queries) == 0 || r.queries[0].Token != "tok_abc" {
		t.Fatalf("token not passed to attribution: %+v", r.queries)
	}
	if st.links["polar:ord_1"] == 0 {
		t.Fatal("commission not linked to the order")
	}
	free := polarOrder("order.paid", "ord_free", 0, 0, map[string]any{"reference_id": "tok_abc"})
	if err := a.HandleWebhook(ctx, free, signPolar(polarKey, "msg_f", now, free)); err != nil || len(m.purchases) != 1 {
		t.Fatalf("$0 order: %v, %d purchases", err, len(m.purchases))
	}
	ref := polarOrder("order.refunded", "ord_1", 4900, 2000, nil)
	if err := a.HandleWebhook(ctx, ref, signPolar(polarKey, "msg_r", now, ref)); err != nil {
		t.Fatal(err)
	}
	if len(m.refunds) != 1 || m.refunds[0].refunded != 2000 || m.refunds[0].original != 4900 || m.refunds[0].parent != 1 {
		t.Fatalf("refund: %+v", m.refunds)
	}
	// A refund that includes tax is capped at what the commission was on.
	full := polarOrder("order.refunded", "ord_1", 4900, 5390, nil)
	if err := a.HandleWebhook(ctx, full, signPolar(polarKey, "msg_r2", now, full)); err != nil {
		t.Fatal(err)
	}
	if m.refunds[1].refunded != 4900 {
		t.Fatalf("full refund: %+v", m.refunds[1])
	}
	miss := polarOrder("order.refunded", "ord_unknown", 1000, 1000, nil)
	if err := a.HandleWebhook(ctx, miss, signPolar(polarKey, "msg_m", now, miss)); err != nil || len(st.misses) != 1 {
		t.Fatalf("unknown order refund: %v %v", err, st.misses)
	}
}
