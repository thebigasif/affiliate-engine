package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// --- Recurly fixtures (XML notifications; root element = event type) ---

func recurlyHeaders(user, pass string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	return h
}

func recurlyInvoiceXML(state, uuid, currency string, totalCents int64) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<closed_invoice_notification>
  <account>
    <account_code>acct-7</account_code>
    <username nil="true"></username>
    <email>verena@example.com</email>
    <first_name>Verena</first_name>
    <last_name>Example</last_name>
  </account>
  <invoice>
    <uuid>%s</uuid>
    <subscription_id nil="true"></subscription_id>
    <state>%s</state>
    <origin>purchase</origin>
    <invoice_number_prefix></invoice_number_prefix>
    <invoice_number type="integer">1000</invoice_number>
    <total_in_cents type="integer">%d</total_in_cents>
    <currency>%s</currency>
    <date type="datetime">2014-01-01T20:20:29Z</date>
    <closed_at type="datetime">2014-01-01T20:24:02Z</closed_at>
  </invoice>
</closed_invoice_notification>`, uuid, state, totalCents, currency))
}

func recurlyChargeInvoiceXML(state, uuid, currency string, totalCents int64) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<paid_charge_invoice_notification>
  <account>
    <account_code>acct-7</account_code>
    <email>verena@example.com</email>
  </account>
  <invoice>
    <uuid>%s</uuid>
    <state>%s</state>
    <origin>renewal</origin>
    <invoice_number_prefix>MA1-</invoice_number_prefix>
    <invoice_number type="integer">1031</invoice_number>
    <total_in_cents type="integer">%d</total_in_cents>
    <currency>%s</currency>
    <date type="datetime">2018-02-13T16:00:04Z</date>
    <closed_at type="datetime">2018-02-13T16:00:04Z</closed_at>
  </invoice>
</paid_charge_invoice_notification>`, uuid, state, totalCents, currency))
}

func recurlyRefundXML(root, txID, invoiceID string, amountCents int64) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<%s>
  <account>
    <account_code>acct-7</account_code>
    <email>verena@example.com</email>
  </account>
  <transaction>
    <id>%s</id>
    <invoice_id>%s</invoice_id>
    <invoice_number_prefix></invoice_number_prefix>
    <invoice_number type="integer">1000</invoice_number>
    <action>credit</action>
    <date type="datetime">2010-10-06T20:37:55Z</date>
    <amount_in_cents type="integer">%d</amount_in_cents>
    <status>success</status>
  </transaction>
</%s>`, root, txID, invoiceID, amountCents, root))
}

func newRecurlyAdapter() (*billing.RecurlyAdapter, *paddleMock, *stubStore) {
	m := &paddleMock{}
	st := newStubStore()
	return billing.NewRecurlyAdapter("u", "p", "", m, nopResolver{}, st), m, st
}

func signRecurly(secret string, tsMillis int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprint(tsMillis)))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("%d,%s", tsMillis, hex.EncodeToString(mac.Sum(nil)))
}

// RV1: closed invoice (collected) -> purchase; minor units as-is; account
// binding rung; linked to invoice uuid + human invoice ref.
func TestRecurly_ClosedInvoicePurchase(t *testing.T) {
	a, mock, store := newRecurlyAdapter()
	body := recurlyInvoiceXML("collected", "ffc64d71d4b5404e93f13aac9c63b007", "USD", 1100)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	p := mock.purchases[0]
	if p.Source != "recurly" || p.AmountMinor != 1100 || p.Currency != "USD" {
		t.Fatalf("event=%+v", p)
	}
	if p.CustomerExtID != "acct-7" || p.CustomerEmail != "verena@example.com" {
		t.Fatalf("event=%+v", p)
	}
	if p.EventType != "purchase" {
		t.Fatalf("EventType=%q", p.EventType)
	}
	if p.OrderID != "1000" {
		t.Fatalf("OrderID=%q want invoice ref 1000", p.OrderID)
	}
	if store.links["recurly:ffc64d71d4b5404e93f13aac9c63b007"] == 0 {
		t.Fatal("invoice uuid not linked")
	}
	if store.links["recurly:1000"] == 0 {
		t.Fatal("invoice ref not linked")
	}
	// OccurredAt: closed_at beats creation date.
	want := time.Date(2014, 1, 1, 20, 24, 2, 0, time.UTC)
	if !p.OccurredAt.Equal(want) {
		t.Fatalf("OccurredAt=%v want closed_at %v", p.OccurredAt, want)
	}
}

// RV2: paid charge invoice (Credit Invoices era) with renewal origin ->
// EventType renewal; prefixed invoice ref in OrderID and links.
func TestRecurly_ChargeInvoiceRenewal(t *testing.T) {
	a, mock, store := newRecurlyAdapter()
	body := recurlyChargeInvoiceXML("paid", "ra8fzs41jdxd", "EUR", 100000)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	p := mock.purchases[0]
	if p.EventType != "renewal" {
		t.Fatalf("EventType=%q want renewal", p.EventType)
	}
	if p.AmountMinor != 100000 || p.Currency != "EUR" {
		t.Fatalf("event=%+v", p)
	}
	if p.OrderID != "MA1-1031" {
		t.Fatalf("OrderID=%q want MA1-1031", p.OrderID)
	}
	if store.links["recurly:MA1-1031"] == 0 {
		t.Fatal("prefixed invoice ref not linked")
	}
}

// RV3: Basic Auth enforcement — missing, wrong password, wrong user, and
// the engine is never called on failure.
func TestRecurly_BasicAuth(t *testing.T) {
	a, mock, _ := newRecurlyAdapter()
	body := recurlyInvoiceXML("collected", "rv3", "USD", 500)
	if err := a.HandleWebhook(context.Background(), body, http.Header{}); err == nil {
		t.Fatal("missing Authorization accepted")
	}
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "WRONG")); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("intruder", "p")); err == nil {
		t.Fatal("wrong username accepted")
	}
	if len(mock.purchases) != 0 {
		t.Fatal("engine called on auth failure")
	}
}

// RV4: recurly-signature verification when a secret is configured —
// valid sig passes, tampered body fails, stale ts fails, wrong secret
// fails, and a signature with no configured secret is refused.
func TestRecurly_Signature(t *testing.T) {
	m := &paddleMock{}
	st := newStubStore()
	a := billing.NewRecurlyAdapter("u", "p", "sec_rv", m, nopResolver{}, st)
	body := recurlyInvoiceXML("collected", "rv4", "USD", 500)
	h := recurlyHeaders("u", "p")

	now := time.Now().UnixMilli()
	h.Set("recurly-signature", signRecurly("sec_rv", now, body))
	if err := a.HandleWebhook(context.Background(), body, h); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if len(m.purchases) != 1 {
		t.Fatalf("purchases=%d", len(m.purchases))
	}

	// Tampered body (signature no longer covers the parsed bytes).
	h2 := recurlyHeaders("u", "p")
	h2.Set("recurly-signature", signRecurly("sec_rv", now, body))
	if err := a.HandleWebhook(context.Background(), []byte(string(body)+" "), h2); err == nil {
		t.Fatal("tampered body accepted")
	}

	// Stale timestamp (outside the 5-minute tolerance).
	h3 := recurlyHeaders("u", "p")
	h3.Set("recurly-signature", signRecurly("sec_rv", now-20*60*1000, body))
	if err := a.HandleWebhook(context.Background(), body, h3); err == nil {
		t.Fatal("stale signature accepted")
	}

	// Wrong secret.
	h4 := recurlyHeaders("u", "p")
	h4.Set("recurly-signature", signRecurly("OTHER", now, body))
	if err := a.HandleWebhook(context.Background(), body, h4); err == nil {
		t.Fatal("wrong secret accepted")
	}

	// Multi-sig header: old+new rotation keys — the second matches.
	oldHdr := signRecurly("oldkey", now, body) // "<ts>,<sig_old>"
	_, newSig, _ := strings.Cut(signRecurly("sec_rv", now, body), ",")
	h5 := recurlyHeaders("u", "p")
	h5.Set("recurly-signature", oldHdr+","+newSig) // ts,sig_old,sig_new
	if err := a.HandleWebhook(context.Background(), body, h5); err != nil {
		t.Fatalf("rotation multi-sig rejected: %v", err)
	}

	// Signature present but adapter has no secret configured: refuse.
	a2, mock2, _ := newRecurlyAdapter()
	h6 := recurlyHeaders("u", "p")
	h6.Set("recurly-signature", signRecurly("whatever", now, body))
	if err := a2.HandleWebhook(context.Background(), body, h6); err == nil {
		t.Fatal("signed delivery against unsigned config accepted")
	}
	if len(mock2.purchases) != 0 {
		t.Fatal("engine called on refused signature")
	}
}

// RV5: uncollected states and zero totals skip silently (failed
// collection fires closed_invoice_notification too).
func TestRecurly_SilentSkips(t *testing.T) {
	a, mock, _ := newRecurlyAdapter()
	for _, state := range []string{"pending", "processing", "past_due", "failed"} {
		body := recurlyInvoiceXML(state, "rv5-"+state, "USD", 100)
		if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	body := recurlyInvoiceXML("collected", "rv5-zero", "USD", 0)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	// Unrelated notification roots: silent success.
	for _, root := range []string{"new_account_notification", "updated_subscription_notification", "new_dunning_event_notification", "item_created_notification"} {
		xml := []byte(fmt.Sprintf(`<?xml version="1.0"?><%s><account><account_code>a</account_code></account></%s>`, root, root))
		if err := a.HandleWebhook(context.Background(), xml, recurlyHeaders("u", "p")); err != nil {
			t.Fatalf("%s: %v", root, err)
		}
	}
	if len(mock.purchases) != 0 {
		t.Fatal("non-collected/zero/unknown commissioned")
	}
}

// RV6: refund finds the parent via invoice uuid (or ref) and claws back
// with the transaction amount; proration base from the store.
func TestRecurly_Refund(t *testing.T) {
	a, mock, store := newRecurlyAdapter()
	purchase := recurlyInvoiceXML("collected", "inv-rv6", "USD", 10000)
	if err := a.HandleWebhook(context.Background(), purchase, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	refund := recurlyRefundXML("successful_refund_notification", "tx-rv6", "inv-rv6", 3000)
	if err := a.HandleWebhook(context.Background(), refund, recurlyHeaders("u", "p")); err != nil {
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
}

// RV7: refund with no parent commission -> RecordRefundMiss, no engine call.
func TestRecurly_RefundMiss(t *testing.T) {
	a, mock, store := newRecurlyAdapter()
	body := recurlyRefundXML("successful_refund_notification", "tx-rv7", "inv-unknown", 500)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(store.misses) != 1 {
		t.Fatalf("misses=%v", store.misses)
	}
	if len(mock.refunds) != 0 {
		t.Fatal("engine refund called on miss")
	}
}

// RV8: void payment (pre-settlement) claws back like a refund; distinct
// idempotency key namespace from refunds.
func TestRecurly_VoidPayment(t *testing.T) {
	a, mock, _ := newRecurlyAdapter()
	purchase := recurlyInvoiceXML("collected", "inv-rv8", "USD", 2000)
	a.HandleWebhook(context.Background(), purchase, recurlyHeaders("u", "p"))
	body := recurlyRefundXML("void_payment_notification", "tx-rv8", "inv-rv8", 2000)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(mock.refunds) != 1 {
		t.Fatalf("refunds=%d", len(mock.refunds))
	}
	if mock.refunds[0].refunded != 2000 {
		t.Fatalf("refunded=%d", mock.refunds[0].refunded)
	}
}

// RV9: replayed delivery dedupes via raw_events (mock contract); a
// reopened+reclosed invoice is a DISTINCT event (closed_at in the key).
func TestRecurly_ReplayAndReopen(t *testing.T) {
	a, mock, _ := newRecurlyAdapter()
	body := recurlyInvoiceXML("collected", "inv-rv9", "USD", 1200)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d after replay", len(mock.purchases))
	}
	// Reopened then reclosed later: different closed_at -> distinct event.
	reclosed := []byte(strings.Replace(string(body),
		"2014-01-01T20:24:02Z", "2014-02-01T10:00:00Z", 1))
	if err := a.HandleWebhook(context.Background(), reclosed, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 2 {
		t.Fatalf("purchases=%d after reopen-reclose (want distinct event)", len(mock.purchases))
	}
}

// RV10: unattributed organic sale -> ACK (nil error), not a retry loop.
func TestRecurly_UnattributedACK(t *testing.T) {
	m := &paddleMock{}
	m.failNoAttribution = true
	a := billing.NewRecurlyAdapter("u", "p", "", m, nopResolver{}, newStubStore())
	body := recurlyInvoiceXML("collected", "inv-rv10", "USD", 900)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatalf("organic sale must ACK, got: %v", err)
	}
}

// RV11: adapter satisfies the normalized Adapter contract.
var _ billing.Adapter = (*billing.RecurlyAdapter)(nil)

// RV12: attribution query carries the customer-binding rung (the only
// rung Recurly XML supports — token/coupon have no carrier).
func TestRecurly_AttributionQuery(t *testing.T) {
	m := &paddleMock{}
	res := &lsRecordingResolver{}
	a := billing.NewRecurlyAdapter("u", "p", "", m, res, newStubStore())
	body := recurlyInvoiceXML("collected", "inv-rv12", "USD", 100)
	if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if len(res.queries) != 1 {
		t.Fatalf("resolve calls=%d", len(res.queries))
	}
	q := res.queries[0]
	if q.CustomerExtID != "acct-7" {
		t.Fatalf("CustomerExtID=%q — binding rung dead", q.CustomerExtID)
	}
	if q.Token != "" || q.CouponCode != "" {
		t.Fatalf("token/coupon must be empty for Recurly: %+v", q)
	}
}

// RV13: garbage XML after valid auth -> malformed (400), not a panic.
func TestRecurly_MalformedXML(t *testing.T) {
	a, _, _ := newRecurlyAdapter()
	if err := a.HandleWebhook(context.Background(), []byte("<not-xml"), recurlyHeaders("u", "p")); err == nil {
		t.Fatal("garbage accepted")
	}
	if err := a.HandleWebhook(context.Background(), []byte("not xml at all"), recurlyHeaders("u", "p")); err == nil {
		t.Fatal("non-xml accepted")
	}
}

// RV14: renewal origin maps to EventType renewal; purchase/immediate/
// one_off all credit as purchase.
func TestRecurly_OriginMapping(t *testing.T) {
	a, mock, _ := newRecurlyAdapter()
	for _, tc := range []struct{ origin, want string }{
		{"purchase", "purchase"},
		{"renewal", "renewal"},
		{"immediate", "purchase"},
		{"one_off", "purchase"},
		{"", "purchase"},
	} {
		base := string(recurlyInvoiceXML("collected", "rv14-"+tc.origin+"-x", "USD", 100))
		body := []byte(strings.Replace(base, "<origin>purchase</origin>",
			"<origin>"+tc.origin+"</origin>", 1))
		// Handle the "" case: the placeholder leaves <origin></origin>.
		if tc.origin == "" {
			body = []byte(strings.Replace(base, "<origin>purchase</origin>", "<origin></origin>", 1))
		}
		if err := a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p")); err != nil {
			t.Fatalf("%q: %v", tc.origin, err)
		}
		got := mock.purchases[len(mock.purchases)-1].EventType
		if got != tc.want {
			t.Fatalf("origin %q -> %q want %q", tc.origin, got, tc.want)
		}
	}
}

// RV15: refund proration base is the SALE gross, not the commission amount.
// Real engine: 10% of 10000 = 1000; a 3000 (30%) refund claws 300 (against
// the commission base the ratio clamped to 100% and wiped the commission).
func TestRecurly_RefundProratesAgainstGross(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewRecurlyAdapter("u", "p", "", engine, mockResolving(1, 1), store)
	purchase := recurlyInvoiceXML("collected", "inv-rv15", "USD", 10000)
	if err := a.HandleWebhook(context.Background(), purchase, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	refund := recurlyRefundXML("successful_refund_notification", "tx-rv15", "inv-rv15", 3000)
	if err := a.HandleWebhook(context.Background(), refund, recurlyHeaders("u", "p")); err != nil {
		t.Fatal(err)
	}
	if _, claw := clawbackFor(t, d); claw != -300 {
		t.Fatalf("clawback=%d want -300 (30%% of the 1000 commission)", claw)
	}
}
