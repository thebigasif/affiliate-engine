package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

const testSecret = "whsec_test_0123456789abcdef"

// --- helpers ---

// sign computes a valid Stripe-Signature header value for the body.
func sign(t string, secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("t=%s,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

func signedHeaders(body []byte) http.Header {
	t := fmt.Sprintf("%d", time.Now().Unix())
	return http.Header{"Stripe-Signature": {sign(t, testSecret, body)}}
}

func headersWith(sig string) http.Header {
	return http.Header{"Stripe-Signature": {sig}}
}

// forge returns a signature computed over a DIFFERENT body but otherwise
// well-formed — simulates a tampered payload, not a malformed header.
func forge(t string, body []byte) string {
	s := sign(t, testSecret, body)
	// flip the last hex char so the v1 stays decodable but wrong
	last := s[len(s)-1]
	if last == '0' {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}

// --- mocks ---

type purchaseCall struct {
	ev commissions.Event
}

type refundCall struct {
	ev                       commissions.Event
	origID, refunded, origAm int64
}

type mockEngine struct {
	purchases   []purchaseCall
	refunds     []refundCall
	nextID      int64
	purchaseErr error
	refundErr   error
}

func (m *mockEngine) ProcessPurchase(ctx context.Context, ev commissions.Event) (int64, error) {
	m.purchases = append(m.purchases, purchaseCall{ev: ev})
	if m.purchaseErr != nil {
		return 0, m.purchaseErr
	}
	if m.nextID == 0 {
		m.nextID = 42
	}
	return m.nextID, nil
}

func (m *mockEngine) ProcessRefund(ctx context.Context, ev commissions.Event, origID, refunded, origAm int64) error {
	m.refunds = append(m.refunds, refundCall{ev: ev, origID: origID, refunded: refunded, origAm: origAm})
	return m.refundErr
}

type mockResolver struct {
	res attribution.Resolution
	err error
	got []attribution.Query
}

func (m *mockResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	m.got = append(m.got, q)
	return m.res, m.err
}

type missRecord struct {
	eventID string
	keys    []string
	amount  int64
}

type mockStore struct {
	links     map[string]int64 // key -> commission id
	misses    []missRecord
	linkCalls int
	findErr   error
}

func (m *mockStore) LinkCommission(ctx context.Context, source, eventID string, commissionID int64, keys ...string) error {
	m.linkCalls++
	if m.links == nil {
		m.links = map[string]int64{}
	}
	for _, k := range keys {
		if _, ok := m.links[k]; !ok { // first link wins
			m.links[k] = commissionID
		}
	}
	return nil
}

func (m *mockStore) FindCommission(ctx context.Context, source string, keys ...string) (int64, bool, error) {
	if m.findErr != nil {
		return 0, false, m.findErr
	}
	for _, k := range keys {
		if id, ok := m.links[k]; ok {
			return id, true, nil
		}
	}
	return 0, false, nil
}

func (m *mockStore) RecordRefundMiss(ctx context.Context, source, refundEventID string, keys []string, amountMinor int64, currency string) error {
	m.misses = append(m.misses, missRecord{eventID: refundEventID, keys: keys, amount: amountMinor})
	return nil
}

func newAdapter(engine billing.ProcessPurchaser, resolver billing.AttributionResolver, store billing.Store) *billing.StripeAdapter {
	return billing.NewStripeAdapter(testSecret, engine, resolver, store)
}

// --- fixtures ---

const checkoutJSON = `{
  "id": "evt_test_1",
  "type": "checkout.session.completed",
  "data": {"object": {
    "id": "cs_test_1",
    "object": "checkout.session",
    "mode": "payment",
    "payment_status": "paid",
    "amount_total": 10000,
    "currency": "usd",
    "customer": "cus_123",
    "payment_intent": "pi_1",
    "created": 1700000000,
    "metadata": {"aff_token": "tok_abc", "coupon": "SAVE10"},
    "total_details": {"amount_discount": 500},
    "discount": [{"coupon": "coupon_stripe_id"}]
  }}
}`

// renewal invoice: subscription_cycle => EventType "renewal";
// total_excluding_tax (11000) preferred over amount_paid (12000);
// subscription-level token + invoice-level coupon merged.
const invoiceRenewalJSON = `{
  "id": "evt_inv_1",
  "type": "invoice.paid",
  "data": {"object": {
    "id": "in_1",
    "amount_paid": 12000,
    "total_excluding_tax": 11000,
    "currency": "usd",
    "customer": "cus_123",
    "payment_intent": "pi_2",
    "billing_reason": "subscription_cycle",
    "created": 1700000100,
    "subscription_details": {"metadata": {"aff_token": "tok_renew"}},
    "metadata": {"coupon": "SUB10"}
  }}
}`

// non-cycle invoice stays a plain purchase.
const invoiceFirstJSON = `{
  "id": "evt_inv_2",
  "type": "invoice.paid",
  "data": {"object": {
    "id": "in_2",
    "amount_paid": 9000,
    "currency": "eur",
    "customer": "cus_9",
    "billing_reason": "subscription_create",
    "created": 1700000150,
    "metadata": {"token": "tok_plain"}
  }}
}`

const refundJSON = `{
  "id": "evt_rf_1",
  "type": "charge.refunded",
  "data": {"object": {
    "id": "ch_1",
    "amount": 10000,
    "amount_refunded": 2500,
    "currency": "usd",
    "customer": "cus_123",
    "payment_intent": "pi_1",
    "invoice": "in_1",
    "created": 1700000200,
    "refunded": true,
    "refunds": {"data": [{"id": "re_1", "amount": 2500, "status": "succeeded"}]}
  }}
}`

const subDeletedJSON = `{
  "id": "evt_sub_1",
  "type": "customer.subscription.deleted",
  "data": {"object": {"id": "sub_1", "customer": "cus_123"}}
}`

const unknownJSON = `{
  "id": "evt_unk",
  "type": "payment_intent.succeeded",
  "data": {"object": {"id": "pi_9"}}
}`

// --- signature verification ---

func TestBadSignatureRejected(t *testing.T) {
	engine := &mockEngine{}
	store := &mockStore{}
	a := newAdapter(engine, nil, store)
	body := []byte(checkoutJSON)

	err := a.HandleWebhook(context.Background(), body, headersWith(forge(fmt.Sprintf("%d", time.Now().Unix()), []byte("other body"))))
	if !errors.Is(err, billing.ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
	// nothing downstream ran — verify happens BEFORE any write
	if len(engine.purchases) != 0 || len(engine.refunds) != 0 || store.linkCalls != 0 {
		t.Fatal("engine/store touched on bad signature")
	}
}

func TestForgedOneCharOffSignatureRejected(t *testing.T) {
	a := newAdapter(&mockEngine{}, nil, nil)
	body := []byte(checkoutJSON)
	tStr := fmt.Sprintf("%d", time.Now().Unix())

	if err := a.HandleWebhook(context.Background(), body, headersWith(forge(tStr, body))); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestMissingOrMalformedSignatureRejected(t *testing.T) {
	a := newAdapter(&mockEngine{}, nil, nil)
	body := []byte(checkoutJSON)

	cases := map[string]http.Header{
		"missing header":  {},
		"empty value":     headersWith(""),
		"no v1":           headersWith(fmt.Sprintf("t=%d", time.Now().Unix())),
		"no t":            headersWith("v1=" + hex.EncodeToString(make([]byte, 32))),
		"garbage":         headersWith("this is not a signature"),
		"v1 not hex":      headersWith(fmt.Sprintf("t=%d,v1=zznothex", time.Now().Unix())),
		"non-numeric t":   headersWith("t=now,v1=" + strings.Repeat("a", 64)),
		"v2-only ignored": headersWith(fmt.Sprintf("t=%d,v2=%s", time.Now().Unix(), strings.Repeat("a", 64))),
	}
	for name, h := range cases {
		if err := a.HandleWebhook(context.Background(), body, h); !errors.Is(err, billing.ErrInvalidSignature) {
			t.Errorf("%s: want ErrInvalidSignature, got %v", name, err)
		}
	}
	if err := a.HandleWebhook(context.Background(), body, nil); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Errorf("nil header: want ErrInvalidSignature, got %v", err)
	}
}

func TestStaleSignatureRejected(t *testing.T) {
	a := newAdapter(&mockEngine{}, nil, nil)
	body := []byte(checkoutJSON)
	old := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	if err := a.HandleWebhook(context.Background(), body, headersWith(sign(old, testSecret, body))); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature for stale t=, got %v", err)
	}
}

func TestRotatedMultipleV1Accepted(t *testing.T) {
	engine := &mockEngine{}
	// old secret still valid during rotation: header carries both v1s
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := []byte(checkoutJSON)
	tStr := fmt.Sprintf("%d", time.Now().Unix())
	h := http.Header{"Stripe-Signature": {sign(tStr, testSecret, body) + "," + sign(tStr, "whsec_old", body)}}
	if err := a.HandleWebhook(context.Background(), body, h); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("want 1 purchase, got %d", len(engine.purchases))
	}
}

// --- event mapping ---

func mockResolving(aff, camp int64) *mockResolver {
	return &mockResolver{res: attribution.Resolution{AffiliateID: aff, CampaignID: camp, Method: "token"}}
}

func TestCheckoutCompletedMapsAndPurchases(t *testing.T) {
	engine := &mockEngine{}
	res := mockResolving(7, 2)
	store := &mockStore{}
	a := newAdapter(engine, res, store)

	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("ProcessPurchase calls = %d, want 1", len(engine.purchases))
	}
	ev := engine.purchases[0].ev
	if ev.Source != "stripe" || ev.ExternalID != "evt_test_1" || ev.EventType != "purchase" {
		t.Errorf("identity mismatch: %+v", ev)
	}
	if ev.AmountMinor != 10000 {
		t.Errorf("AmountMinor = %d, want 10000 (minor units, no conversion)", ev.AmountMinor)
	}
	if ev.Currency != "USD" {
		t.Errorf("Currency = %q, want uppercased USD", ev.Currency)
	}
	if ev.CustomerExtID != "cus_123" || ev.ClickToken != "tok_abc" || ev.CouponCode != "SAVE10" {
		t.Errorf("attribution fields mismatch: %+v", ev)
	}
	if ev.OrderID != "pi_1" {
		t.Errorf("OrderID = %q, want pi_1 (payment intent as order reference)", ev.OrderID)
	}
	if ev.AffiliateID != 7 || ev.CampaignID != 2 {
		t.Errorf("pre-resolved affiliate/campaign = %d/%d, want 7/2", ev.AffiliateID, ev.CampaignID)
	}
	if !ev.OccurredAt.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("OccurredAt = %v, want object.created", ev.OccurredAt)
	}

	// resolver got the query built from metadata
	if len(res.got) != 1 || res.got[0].Token != "tok_abc" || res.got[0].CouponCode != "SAVE10" ||
		res.got[0].CustomerExtID != "cus_123" || res.got[0].IPHash != nil {
		t.Errorf("resolver query mismatch: %+v", res.got)
	}

	// linkage recorded under cs_ + pi_ aliases
	if store.links["cs_test_1"] != 42 || store.links["pi_1"] != 42 {
		t.Errorf("link aliases = %v, want cs_test_1/pi_1 -> 42", store.links)
	}
}

func TestInvoicePaidRenewalMapping(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})

	if err := a.HandleWebhook(context.Background(), []byte(invoiceRenewalJSON), signedHeaders([]byte(invoiceRenewalJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("ProcessPurchase calls = %d, want 1", len(engine.purchases))
	}
	ev := engine.purchases[0].ev
	if ev.EventType != "renewal" {
		t.Errorf("EventType = %q, want renewal for subscription_cycle", ev.EventType)
	}
	if ev.AmountMinor != 11000 {
		t.Errorf("AmountMinor = %d, want 11000 (total_excluding_tax preferred over amount_paid 12000)", ev.AmountMinor)
	}
	if ev.ClickToken != "tok_renew" || ev.CouponCode != "SUB10" {
		t.Errorf("merged metadata mismatch: token=%q coupon=%q", ev.ClickToken, ev.CouponCode)
	}
	if ev.OrderID != "pi_2" {
		t.Errorf("OrderID = %q, want pi_2 (invoice payment_intent)", ev.OrderID)
	}
}

// TestTokenResolutionCarriesClickID: the ladder's token rung identifies
// the originating click — that id must ride the event into the commission
// row (postback {click_id}, FR-7).
func TestTokenResolutionCarriesClickID(t *testing.T) {
	engine := &mockEngine{}
	res := &mockResolver{res: attribution.Resolution{
		AffiliateID: 7, CampaignID: 2, Method: "token", ClickID: "clk_token_1",
	}}
	a := newAdapter(engine, res, &mockStore{})

	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("ProcessPurchase calls = %d, want 1", len(engine.purchases))
	}
	if ev := engine.purchases[0].ev; ev.ClickID != "clk_token_1" {
		t.Fatalf("ClickID = %q, want clk_token_1 (token-rung linkage)", ev.ClickID)
	}
}

func TestInvoicePaidFirstCycleIsPurchase(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})

	if err := a.HandleWebhook(context.Background(), []byte(invoiceFirstJSON), signedHeaders([]byte(invoiceFirstJSON))); err != nil {
		t.Fatal(err)
	}
	ev := engine.purchases[0].ev
	if ev.EventType != "purchase" {
		t.Errorf("EventType = %q, want purchase for subscription_create", ev.EventType)
	}
	if ev.AmountMinor != 9000 {
		t.Errorf("AmountMinor = %d, want 9000 (amount_paid fallback)", ev.AmountMinor)
	}
	if ev.ClickToken != "tok_plain" {
		t.Errorf("token fallback alias (metadata.token) = %q, want tok_plain", ev.ClickToken)
	}
	if ev.Currency != "EUR" {
		t.Errorf("Currency = %q, want EUR", ev.Currency)
	}
}

func TestRefundMapsToProcessRefundWithRatioArgs(t *testing.T) {
	engine := &mockEngine{}
	store := &mockStore{links: map[string]int64{"pi_1": 42}}
	a := newAdapter(engine, nil, store)

	if err := a.HandleWebhook(context.Background(), []byte(refundJSON), signedHeaders([]byte(refundJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.refunds) != 1 {
		t.Fatalf("ProcessRefund calls = %d, want 1", len(engine.refunds))
	}
	call := engine.refunds[0]
	if call.origID != 42 {
		t.Errorf("originalCommissionID = %d, want 42 (linked via pi_1)", call.origID)
	}
	if call.refunded != 2500 || call.origAm != 10000 {
		t.Errorf("ratio args = %d/%d, want 2500/10000", call.refunded, call.origAm)
	}
	if call.ev.EventType != "refund" || call.ev.AmountMinor != 2500 || call.ev.Source != "stripe" || call.ev.ExternalID != "evt_rf_1" {
		t.Errorf("refund event mismatch: %+v", call.ev)
	}
	if len(engine.purchases) != 0 {
		t.Error("refund must not call ProcessPurchase")
	}
}

func TestRefundWithoutParentRecordedAndSkipped(t *testing.T) {
	engine := &mockEngine{}
	store := &mockStore{}
	a := newAdapter(engine, nil, store)

	if err := a.HandleWebhook(context.Background(), []byte(refundJSON), signedHeaders([]byte(refundJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.refunds) != 0 {
		t.Fatal("ProcessRefund must not run without a linked parent")
	}
	if len(store.misses) != 1 || store.misses[0].eventID != "evt_rf_1" || store.misses[0].amount != 2500 {
		t.Fatalf("refund miss not recorded: %+v", store.misses)
	}
}

func TestRefundStoreErrorSurfaces(t *testing.T) {
	engine := &mockEngine{}
	store := &mockStore{findErr: sql.ErrConnDone}
	a := newAdapter(engine, nil, store)
	if err := a.HandleWebhook(context.Background(), []byte(refundJSON), signedHeaders([]byte(refundJSON))); err == nil {
		t.Fatal("transient store failure must surface so the provider retries")
	}
	if len(engine.refunds) != 0 {
		t.Error("refund must not run when linkage lookup fails")
	}
}

// --- idempotency / replays ---

func TestReplayIsSuccessNoOp(t *testing.T) {
	engine := &mockEngine{purchaseErr: commissions.ErrAlreadyProcessed}
	store := &mockStore{}
	a := newAdapter(engine, mockResolving(7, 2), store)

	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatalf("replay must surface as success no-op, got %v", err)
	}
	if store.linkCalls != 0 {
		t.Error("no linkage should happen on replay")
	}

	// replayed refund event likewise
	rEngine := &mockEngine{refundErr: commissions.ErrAlreadyProcessed}
	rStore := &mockStore{links: map[string]int64{"pi_1": 42}}
	a2 := newAdapter(rEngine, nil, rStore)
	if err := a2.HandleWebhook(context.Background(), []byte(refundJSON), signedHeaders([]byte(refundJSON))); err != nil {
		t.Fatalf("refund replay must surface as success no-op, got %v", err)
	}
}

func TestUnattributedIsAckedNotErrored(t *testing.T) {
	engine := &mockEngine{purchaseErr: commissions.ErrNoAttribution}
	a := newAdapter(engine, &mockResolver{}, &mockStore{})
	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatalf("unattributed is permanent: want ACK (nil), got %v", err)
	}
}

// --- attribution pre-resolve ---

func TestFlaggedIPFallbackSkipped(t *testing.T) {
	engine := &mockEngine{}
	res := &mockResolver{res: attribution.Resolution{AffiliateID: 9, Method: "ip_fallback", Flagged: true}}
	a := newAdapter(engine, res, nil)

	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 0 {
		t.Fatal("flagged (ip_fallback) resolution must skip processing, conservative default")
	}
}

func TestResolverErrorFallsThroughToEngine(t *testing.T) {
	engine := &mockEngine{}
	res := &mockResolver{err: errors.New("db down")}
	a := newAdapter(engine, res, nil)
	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatal("resolver error must fall through to the engine, not drop the event")
	}
	if engine.purchases[0].ev.AffiliateID != 0 {
		t.Error("no affiliate stamp on resolver failure")
	}
}

func TestNilResolverFallsThroughToEngine(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, nil, nil)
	if err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatal("nil resolver must not block processing")
	}
}

// --- misc events ---

func TestUnknownEventTypeIgnored(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, nil, nil)
	if err := a.HandleWebhook(context.Background(), []byte(unknownJSON), signedHeaders([]byte(unknownJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases)+len(engine.refunds) != 0 {
		t.Fatal("unknown event type must not touch the engine")
	}
}

func TestSubscriptionDeletedRecordedOnly(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, nil, nil)
	if err := a.HandleWebhook(context.Background(), []byte(subDeletedJSON), signedHeaders([]byte(subDeletedJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases)+len(engine.refunds) != 0 {
		t.Fatal("subscription deletion is record-only")
	}
}

func TestMalformedBodyWithValidSignatureRejected(t *testing.T) {
	a := newAdapter(&mockEngine{}, nil, nil)
	body := []byte(`{"id":"evt_x","type":}`) // invalid JSON
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); !errors.Is(err, billing.ErrMalformedEvent) {
		t.Fatalf("want ErrMalformedEvent, got %v", err)
	}
	// signed but incomplete envelope
	body2 := []byte(`{"id":"evt_x"}`)
	if err := a.HandleWebhook(context.Background(), body2, signedHeaders(body2)); !errors.Is(err, billing.ErrMalformedEvent) {
		t.Fatalf("want ErrMalformedEvent for missing type/object, got %v", err)
	}
}

func TestEmptySecretConfigError(t *testing.T) {
	a := billing.NewStripeAdapter("", &mockEngine{}, nil, nil)
	err := a.HandleWebhook(context.Background(), []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON)))
	if err == nil || !strings.Contains(err.Error(), "secret not configured") {
		t.Fatalf("want config error, got %v", err)
	}
}

// --- duplicate-key parsing note ---

// The signature is computed over the RAW bytes; parsing the SAME bytes means
// an attacker cannot make signed and parsed payloads diverge. This test
// pins that behaviour: a duplicate-key body is only accepted when signed
// exactly as delivered, and the last key wins (documented Go behaviour).
func TestDuplicateKeyBodyParsedAsSigned(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := []byte(`{"id":"evt_dup","type":"checkout.session.completed","id":"evt_dup2",` +
		`"data":{"object":{"id":"cs_9","payment_status":"paid","amount_total":5000,"currency":"usd","created":1700000000}}}`)

	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 || engine.purchases[0].ev.ExternalID != "evt_dup2" {
		t.Fatalf("last-key-wins not observed: %+v", engine.purchases)
	}
	// and the same body with a signature for different bytes is rejected
	if err := a.HandleWebhook(context.Background(), body, signedHeaders([]byte(checkoutJSON))); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Fatal("signature over different bytes must be rejected")
	}
}

// --- checkout payment state (async methods, subscriptions) ---

// checkoutBody builds a signed-ready checkout.session event with the given
// type/mode/payment_status.
func checkoutBody(evtID, evtType, sessionID, mode, paymentStatus string, amount int64) []byte {
	return []byte(fmt.Sprintf(`{
  "id": %q,
  "type": %q,
  "data": {"object": {
    "id": %q,
    "object": "checkout.session",
    "mode": %q,
    "payment_status": %q,
    "amount_total": %d,
    "currency": "usd",
    "customer": "cus_async",
    "customer_email": "prefill@example.com",
    "customer_details": {"email": "buyer@example.com", "address": {"country": "de"}},
    "payment_intent": "pi_async",
    "created": 1700000000,
    "metadata": {"aff_token": "tok_async"}
  }}
}`, evtID, evtType, sessionID, mode, paymentStatus, amount))
}

// An async method (SEPA/ACH/OXXO) completes the session "unpaid": no credit
// until checkout.session.async_payment_succeeded, which credits once.
func TestCheckoutUnpaidThenAsyncSucceeded(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})

	unpaid := checkoutBody("evt_cs_unpaid", "checkout.session.completed", "cs_async", "payment", "unpaid", 10000)
	if err := a.HandleWebhook(context.Background(), unpaid, signedHeaders(unpaid)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 0 {
		t.Fatal("unpaid checkout.session.completed credited (async payment may never land)")
	}

	failed := checkoutBody("evt_cs_failed", "checkout.session.async_payment_failed", "cs_async", "payment", "unpaid", 10000)
	if err := a.HandleWebhook(context.Background(), failed, signedHeaders(failed)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 0 {
		t.Fatal("async_payment_failed credited")
	}

	paid := checkoutBody("evt_cs_paid", "checkout.session.async_payment_succeeded", "cs_async", "payment", "paid", 10000)
	if err := a.HandleWebhook(context.Background(), paid, signedHeaders(paid)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("async_payment_succeeded: purchases=%d want 1", len(engine.purchases))
	}
	if ev := engine.purchases[0].ev; ev.AmountMinor != 10000 || ev.ExternalID != "evt_cs_paid" {
		t.Fatalf("event=%+v", ev)
	}
}

// Every non-"paid" status is skipped, including no_payment_required ($0
// trials/100% coupons) and an absent field.
func TestCheckoutNonPaidStatusesSkipped(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	for i, st := range []string{"unpaid", "no_payment_required", ""} {
		body := checkoutBody(fmt.Sprintf("evt_np_%d", i), "checkout.session.completed", "cs_np", "payment", st, 10000)
		if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.purchases) != 0 {
		t.Fatalf("non-paid sessions credited: %d", len(engine.purchases))
	}
}

// Subscription-mode sessions are credited by the first invoice.paid; the
// session itself must not credit (it double-paid every subscription sale).
func TestCheckoutSubscriptionModeNotCredited(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := checkoutBody("evt_cs_sub", "checkout.session.completed", "cs_sub", "subscription", "paid", 9000)
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 0 {
		t.Fatal("subscription-mode checkout credited (the first invoice.paid already covers it)")
	}
	if err := a.HandleWebhook(context.Background(), []byte(invoiceFirstJSON), signedHeaders([]byte(invoiceFirstJSON))); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 1 {
		t.Fatalf("subscription credit: purchases=%d want exactly 1 (the invoice)", len(engine.purchases))
	}
}

// Buyer email comes from customer_details.email (what the buyer typed),
// falling back to customer_email (the prefill); country from
// customer_details.address.country.
func TestCheckoutCustomerDetails(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	body := checkoutBody("evt_cd_1", "checkout.session.completed", "cs_cd", "payment", "paid", 5000)
	if err := a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatal(err)
	}
	if ev := engine.purchases[0].ev; ev.CustomerEmail != "buyer@example.com" || ev.Country != "DE" {
		t.Fatalf("email=%q country=%q, want buyer@example.com/DE", ev.CustomerEmail, ev.Country)
	}

	// No customer_details: prefill fallback.
	fallback := []byte(`{"id":"evt_cd_2","type":"checkout.session.completed","data":{"object":{` +
		`"id":"cs_cd2","mode":"payment","payment_status":"paid","amount_total":5000,"currency":"usd",` +
		`"customer_email":"prefill@example.com","created":1700000000}}}`)
	if err := a.HandleWebhook(context.Background(), fallback, signedHeaders(fallback)); err != nil {
		t.Fatal(err)
	}
	if ev := engine.purchases[1].ev; ev.CustomerEmail != "prefill@example.com" || ev.Country != "" {
		t.Fatalf("fallback email=%q country=%q", ev.CustomerEmail, ev.Country)
	}
}

// Zero/negative gross never reaches the engine (defense in depth).
func TestStripeZeroAmountNotCredited(t *testing.T) {
	engine := &mockEngine{}
	a := newAdapter(engine, mockResolving(7, 2), &mockStore{})
	cs := checkoutBody("evt_z_1", "checkout.session.completed", "cs_z", "payment", "paid", 0)
	if err := a.HandleWebhook(context.Background(), cs, signedHeaders(cs)); err != nil {
		t.Fatal(err)
	}
	inv := []byte(`{"id":"evt_z_2","type":"invoice.paid","data":{"object":{` +
		`"id":"in_z","amount_paid":0,"total_excluding_tax":0,"currency":"usd","customer":"cus_z",` +
		`"billing_reason":"subscription_create","created":1700000000}}}`)
	if err := a.HandleWebhook(context.Background(), inv, signedHeaders(inv)); err != nil {
		t.Fatal(err)
	}
	if len(engine.purchases) != 0 {
		t.Fatalf("zero-amount purchases reached the engine: %d", len(engine.purchases))
	}
}

// --- SQLStore against the real schema ---

func newSQLStore(t *testing.T) (*billing.SQLStore, *db.Handles) {
	t.Helper()
	h := dbtest.Open(t)
	return billing.NewSQLStore(h.DB), h
}

func TestSQLStoreLinkAndFind(t *testing.T) {
	store, _ := newSQLStore(t)
	ctx := context.Background()

	if err := store.LinkCommission(ctx, "stripe", "evt_a", 42, "cs_1", "pi_1"); err != nil {
		t.Fatal(err)
	}
	// duplicate link (first wins) must not error
	if err := store.LinkCommission(ctx, "stripe", "evt_b", 99, "pi_1"); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"cs_1", "pi_1"} {
		id, found, err := store.FindCommission(ctx, "stripe", k)
		if err != nil || !found || id != 42 {
			t.Errorf("key %s: id=%d found=%v err=%v, want 42/true/nil", k, id, found, err)
		}
	}
	if _, found, err := store.FindCommission(ctx, "stripe", "ch_unknown"); err != nil || found {
		t.Errorf("unknown key: found=%v err=%v, want false/nil", found, err)
	}
	// other source namespace is isolated
	if _, found, _ := store.FindCommission(ctx, "paddle", "pi_1"); found {
		t.Error("sources must be namespaced")
	}
}

func TestSQLStoreRefundMiss(t *testing.T) {
	store, h := newSQLStore(t)
	ctx := context.Background()
	if err := store.RecordRefundMiss(ctx, "stripe", "evt_rf_x", []string{"pi_9"}, 1234, "USD"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM webhook_events
		WHERE source='stripe' AND external_event_id='refund_miss:evt_rf_x'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("miss row n=%d err=%v", n, err)
	}
	// a miss record must never satisfy a commission lookup
	if _, found, err := store.FindCommission(ctx, "stripe", "pi_9"); err != nil || found {
		t.Errorf("miss record leaked into lookup: found=%v err=%v", found, err)
	}
}

// --- full wiring: real Engine + real SQLStore, mocked resolver ---

func TestEndToEndPurchaseThenRefundWithRealEngine(t *testing.T) {
	store, h := newSQLStore(t)
	ctx := context.Background()

	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`,
		`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		 VALUES (1, NULL, 'purchase', 'percent', 1000)`, // 10%
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	engine := commissions.NewEngine(h.DB, p)
	resolver := mockResolving(1, 1)
	a := billing.NewStripeAdapter(testSecret, engine, resolver, store)

	// 1) purchase: 10% of 10000 = 1000 commission
	if err := a.HandleWebhook(ctx, []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatal(err)
	}
	var cid, amount int64
	var status string
	if err := h.DB.QueryRow(
		`SELECT id, amount, status FROM commissions WHERE status != 'cancelled'`).Scan(&cid, &amount, &status); err != nil {
		t.Fatalf("commission not credited: %v", err)
	}
	if amount != 1000 || status != "pending" {
		t.Fatalf("commission amount=%d status=%s, want 1000/pending", amount, status)
	}

	// 2) replay the SAME envelope id: no double credit, success no-op
	if err := a.HandleWebhook(ctx, []byte(checkoutJSON), signedHeaders([]byte(checkoutJSON))); err != nil {
		t.Fatalf("replay errored: %v", err)
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("commissions after replay = %d (err=%v), want 1", n, err)
	}

	// 3) refund 2500/10000: clawback = 1000 * 0.25 = 250. A PARTIAL refund
	// claws its share and leaves the parent in its state (pending): only a
	// refund that exhausts the parent moves it to cancelled, and later
	// partial refunds claw their own share from the 750 left in HOLD.
	if err := a.HandleWebhook(ctx, []byte(refundJSON), signedHeaders([]byte(refundJSON))); err != nil {
		t.Fatal(err)
	}
	var claw int64
	if err := h.DB.QueryRow(
		`SELECT amount FROM commissions WHERE parent_commission_id = ?`, cid).Scan(&claw); err != nil {
		t.Fatalf("clawback row missing: %v", err)
	}
	if claw != -250 {
		t.Fatalf("clawback = %d, want -250", claw)
	}
	var parentStatus string
	if err := h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&parentStatus); err != nil || parentStatus != "pending" {
		t.Fatalf("parent status = %q (err=%v), want pending (partial refund does not exhaust the parent)", parentStatus, err)
	}
	if hold, err := p.AccountBalance(ctx, "HOLD:USD"); err != nil || hold != -750 {
		t.Fatalf("HOLD = %d (err=%v), want -750 (1000 credited - 250 clawed)", hold, err)
	}

	// 4) forged signature on the real engine: no writes at all
	before := tableCount(t, h.DB, "raw_events") + tableCount(t, h.DB, "webhook_events")
	body := []byte(checkoutJSON)
	if err := a.HandleWebhook(ctx, body, headersWith(forge(fmt.Sprintf("%d", time.Now().Unix()), body))); !errors.Is(err, billing.ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
	after := tableCount(t, h.DB, "raw_events") + tableCount(t, h.DB, "webhook_events")
	if before != after {
		t.Fatalf("signature failure wrote to DB: before=%d after=%d", before, after)
	}
}

func tableCount(t *testing.T, d *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := d.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
