package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// --- Paddle test harness ---

type paddleMock struct {
	purchases []commissions.Event
	refunds   []paddleRefund
	failNext  bool
	// failNoAttribution makes ProcessPurchase return
	// commissions.ErrNoAttribution (organic-sale simulation).
	failNoAttribution bool
	// processed makes the mock honor the engine's raw_events idempotency:
	// a replayed ExternalID returns commissions.ErrAlreadyProcessed.
	processed map[string]bool
}
type paddleRefund struct {
	ev                 commissions.Event
	parent             int64
	refunded, original int64
}

func (m *paddleMock) ProcessPurchase(ctx context.Context, ev commissions.Event) (int64, error) {
	if m.failNext {
		return 0, fmt.Errorf("engine down")
	}
	if m.failNoAttribution {
		return 0, commissions.ErrNoAttribution
	}
	if m.processed[ev.ExternalID] {
		return 0, commissions.ErrAlreadyProcessed
	}
	if m.processed == nil {
		m.processed = map[string]bool{}
	}
	m.processed[ev.ExternalID] = true
	m.purchases = append(m.purchases, ev)
	return int64(len(m.purchases)), nil
}

func (m *paddleMock) ProcessRefund(ctx context.Context, ev commissions.Event, parent, refunded, original int64) error {
	m.refunds = append(m.refunds, paddleRefund{ev, parent, refunded, original})
	return nil
}

func signPaddle(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprint(ts) + ":"))
	mac.Write(body)
	return fmt.Sprintf("ts=%d;h1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func paddleEnvelope(eventType, eventID string, data any) []byte {
	d, _ := json.Marshal(data)
	b, _ := json.Marshal(map[string]any{
		"event_id": eventID, "event_type": eventType, "data": data,
	})
	_ = d
	return b
}

func paddleHeaders(sig string) http.Header {
	h := http.Header{}
	h.Set("Paddle-Signature", sig)
	return h
}

func newPaddleAdapter(t *testing.T) (*billing.PaddleAdapter, *paddleMock, *stubStore) {
	t.Helper()
	mock := &paddleMock{}
	store := newStubStore()
	adapter := billing.NewPaddleAdapter("whsec_paddle_test", mock, nopResolver{}, store)
	return adapter, mock, store
}

type nopResolver struct{}

func (nopResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	return attribution.Resolution{}, nil
}

// newStubStore: minimal Store for tests (no DB).
type stubStore struct {
	links  map[string]int64 // key -> commission id
	misses []string
}

func newStubStore() *stubStore { return &stubStore{links: map[string]int64{}} }

func (s *stubStore) LinkCommission(ctx context.Context, source, eventID string, commissionID int64, keys ...string) error {
	for _, k := range keys {
		if _, exists := s.links[source+":"+k]; !exists {
			s.links[source+":"+k] = commissionID
		}
	}
	return nil
}

func (s *stubStore) FindCommission(ctx context.Context, source string, keys ...string) (int64, bool, error) {
	for _, k := range keys {
		if id, ok := s.links[source+":"+k]; ok {
			return id, true, nil
		}
	}
	return 0, false, nil
}

func (s *stubStore) RecordRefundMiss(ctx context.Context, source, refundEventID string, keys []string, amountMinor int64, currency string) error {
	s.misses = append(s.misses, refundEventID)
	return nil
}

// newRealMoney wires a REAL commissions.Engine + SQLStore on a migrated
// SQLite database with affiliate 1 and a 10% campaign-default rule (the
// stripe end-to-end pattern). Used to pin refund proration against the
// commission's stored sale gross.
func newRealMoney(t *testing.T) (*commissions.Engine, *billing.SQLStore, *sql.DB) {
	t.Helper()
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
	return commissions.NewEngine(h.DB, p), store, h.DB
}

// clawbackFor returns the (single) credited commission id and the amount of
// its clawback row (0 when none).
func clawbackFor(t *testing.T, d *sql.DB) (cid, claw int64) {
	t.Helper()
	if err := d.QueryRow(`SELECT id FROM commissions WHERE parent_commission_id IS NULL`).Scan(&cid); err != nil {
		t.Fatalf("parent commission: %v", err)
	}
	if err := d.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM commissions WHERE parent_commission_id = ?`, cid).Scan(&claw); err != nil {
		t.Fatalf("clawback: %v", err)
	}
	return cid, claw
}

func paddleSend(t *testing.T, a *billing.PaddleAdapter, body []byte) {
	t.Helper()
	if err := a.HandleWebhook(context.Background(), body,
		paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), body))); err != nil {
		t.Fatal(err)
	}
}

// --- tests ---

// PV1: valid transaction.completed -> purchase with token attribution,
// keyed on the TRANSACTION id, commission linked to the transaction id.
func TestPaddle_ValidTransaction(t *testing.T) {
	a, mock, store := newPaddleAdapter(t)
	body := paddleEnvelope("transaction.completed", "evt_1", map[string]any{
		"id": "txn_1", "customer_id": "ctm_1", "currency_code": "USD",
		"totals":      map[string]string{"grand_total": "4900"},
		"custom_data": map[string]string{"token": "tok_abc"},
	})
	now := time.Now().Unix()
	err := a.HandleWebhook(context.Background(), body, paddleHeaders(signPaddle("whsec_paddle_test", now, body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	p := mock.purchases[0]
	if p.Source != "paddle" || p.ExternalID != "txn_1" || p.AmountMinor != 4900 {
		t.Fatalf("event=%+v (want ExternalID=txn_1: idempotency on the transaction)", p)
	}
	if store.links["paddle:txn_1"] == 0 {
		t.Fatal("transaction id not linked")
	}
}

// PV2: bad/missing/forged signatures rejected before any engine call.
func TestPaddle_SignatureEnforced(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	body := paddleEnvelope("transaction.completed", "evt_2", map[string]any{
		"id": "txn_2", "currency_code": "USD", "totals": map[string]string{"grand_total": "100"},
	})
	now := time.Now().Unix()
	cases := map[string]http.Header{
		"missing":      {},
		"garbage":      paddleHeaders("not-a-sig"),
		"wrong-secret": paddleHeaders(signPaddle("whsec_OTHER", now, body)),
		"stale":        paddleHeaders(signPaddle("whsec_paddle_test", now-3600, body)),
		"tampered":     paddleHeaders(signPaddle("whsec_paddle_test", now, append(append([]byte{}, body[:len(body)-2]...), '}'))),
		"empty-h1":     paddleHeaders(fmt.Sprintf("ts=%d;h1=", now)),
	}
	for name, h := range cases {
		if err := a.HandleWebhook(context.Background(), body, h); err == nil {
			t.Fatalf("%s signature accepted", name)
		}
	}
	if len(mock.purchases) != 0 {
		t.Fatal("engine called despite bad signature")
	}
}

// PV2b: secret rotation — Paddle sends one h1 per active secret; any match
// verifies, in any position.
func TestPaddle_RotatedMultipleH1Accepted(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	body := paddleEnvelope("transaction.completed", "evt_rot", map[string]any{
		"id": "txn_rot", "currency_code": "USD", "totals": map[string]string{"grand_total": "100"},
	})
	now := time.Now().Unix()
	oldSig := signPaddle("whsec_OLD", now, body)         // ts=..;h1=<old>
	newSig := signPaddle("whsec_paddle_test", now, body) // ts=..;h1=<new>
	newH1 := newSig[len(fmt.Sprintf("ts=%d;", now)):]    // "h1=<new>"
	if err := a.HandleWebhook(context.Background(), body, paddleHeaders(oldSig+";"+newH1)); err != nil {
		t.Fatalf("rotation header (old;new) rejected: %v", err)
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	// Neither h1 valid: rejected.
	oldOnly := signPaddle("whsec_OLD", now, body)
	other := signPaddle("whsec_OTHER", now, body)[len(fmt.Sprintf("ts=%d;", now)):]
	if err := a.HandleWebhook(context.Background(), body, paddleHeaders(oldOnly+";"+other)); err == nil {
		t.Fatal("header with no matching h1 accepted")
	}
}

// PV3: approved adjustment refund finds the parent via transaction-id link
// and prorates correctly.
func TestPaddle_RefundFlow(t *testing.T) {
	a, mock, store := newPaddleAdapter(t)
	// Purchase first (creates the link).
	body := paddleEnvelope("transaction.completed", "evt_3", map[string]any{
		"id": "txn_3", "currency_code": "USD",
		"totals": map[string]string{"grand_total": "10000"},
	})
	now := time.Now().Unix()
	if err := a.HandleWebhook(context.Background(), body, paddleHeaders(signPaddle("whsec_paddle_test", now, body))); err != nil {
		t.Fatal(err)
	}
	// Refund 25% (adjustment total is negative in Paddle).
	refund := paddleEnvelope("adjustment.created", "evt_4", map[string]any{
		"id": "adj_1", "transaction_id": "txn_3", "action": "refund", "status": "approved",
		"totals": map[string]string{"total": "-2500"},
	})
	if err := a.HandleWebhook(context.Background(), refund, paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), refund))); err != nil {
		t.Fatal(err)
	}
	if len(mock.refunds) != 1 {
		t.Fatalf("refunds=%d misses=%v", len(mock.refunds), store.misses)
	}
	r := mock.refunds[0]
	if r.parent != 1 || r.refunded != 2500 {
		t.Fatalf("refund call: parent=%d refunded=%d", r.parent, r.refunded)
	}
	if r.ev.ExternalID != "adj_1" {
		t.Fatalf("refund ExternalID=%q want adjustment id adj_1", r.ev.ExternalID)
	}
}

// PV3b: refunds are requested pending_approval and may be rejected — only an
// APPROVED adjustment claws. adjustment.updated carries the approval.
func TestPaddle_AdjustmentOnlyWhenApproved(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	paddleSend(t, a, paddleEnvelope("transaction.completed", "evt_ap1", map[string]any{
		"id": "txn_ap", "currency_code": "USD", "totals": map[string]string{"grand_total": "10000"},
	}))
	for i, status := range []string{"pending_approval", "rejected", "", "reversed"} {
		paddleSend(t, a, paddleEnvelope("adjustment.created", fmt.Sprintf("evt_ap_na_%d", i), map[string]any{
			"id": "adj_ap", "transaction_id": "txn_ap", "action": "refund", "status": status,
			"totals": map[string]string{"total": "-10000"},
		}))
	}
	if len(mock.refunds) != 0 {
		t.Fatalf("non-approved adjustments clawed: %+v", mock.refunds)
	}
	paddleSend(t, a, paddleEnvelope("adjustment.updated", "evt_ap_ok", map[string]any{
		"id": "adj_ap", "transaction_id": "txn_ap", "action": "refund", "status": "approved",
		"totals": map[string]string{"total": "-10000"},
	}))
	if len(mock.refunds) != 1 || mock.refunds[0].refunded != 10000 {
		t.Fatalf("approved adjustment.updated: refunds=%+v", mock.refunds)
	}
}

// PV4: replay of the same envelope is a no-op (engine contract).
func TestPaddle_Replay(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	body := paddleEnvelope("transaction.completed", "evt_5", map[string]any{
		"id": "txn_5", "currency_code": "USD", "totals": map[string]string{"grand_total": "100"},
	})
	h := paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), body))
	if err := a.HandleWebhook(context.Background(), body, h); err != nil {
		t.Fatal(err)
	}
	mock.failNext = false
	// Second delivery: engine would return ErrAlreadyProcessed; adapter maps
	// to nil. (Simulated: mock returns the error once, adapter swallows.)
	if err := a.HandleWebhook(context.Background(), body, h); err != nil {
		t.Fatalf("replay surfaced error: %v", err)
	}
}

// PV4b: two DIFFERENT notifications (distinct event ids) for the same
// transaction credit once — the idempotency key is the transaction id.
func TestPaddle_SameTransactionDistinctEventsDedupes(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	for _, evID := range []string{"evt_dup_a", "evt_dup_b"} {
		paddleSend(t, a, paddleEnvelope("transaction.completed", evID, map[string]any{
			"id": "txn_dup", "currency_code": "USD", "totals": map[string]string{"grand_total": "5000"},
		}))
	}
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d — same transaction credited per notification", len(mock.purchases))
	}
}

// PV5: unknown event types and zero totals are silent successes.
func TestPaddle_SilentSkips(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	for _, et := range []string{"subscription.updated", "customer.created", "report.ready"} {
		body := paddleEnvelope(et, "evt_x_"+et, map[string]any{})
		h := paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), body))
		if err := a.HandleWebhook(context.Background(), body, h); err != nil {
			t.Fatalf("%s: %v", et, err)
		}
	}
	zero := paddleEnvelope("transaction.completed", "evt_zero", map[string]any{
		"id": "txn_z", "currency_code": "USD", "totals": map[string]string{"grand_total": "0"},
	})
	h := paddleHeaders(signPaddle("whsec_paddle_test", time.Now().Unix(), zero))
	if err := a.HandleWebhook(context.Background(), zero, h); err != nil {
		t.Fatal(err)
	}
	if len(mock.purchases) != 0 {
		t.Fatal("zero-total commissioned")
	}
}

// PV6: only transaction.completed is money that landed. ready/paid/billed/
// created fire for checkouts that may never complete (or alongside the
// completed event) and must not credit.
func TestPaddle_OnlyCompletedCredits(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	for _, et := range []string{"transaction.ready", "transaction.paid", "transaction.billed", "transaction.created", "transaction.updated"} {
		paddleSend(t, a, paddleEnvelope(et, "evt_"+et, map[string]any{
			"id": "txn_nc", "currency_code": "USD", "totals": map[string]string{"grand_total": "9900"},
		}))
	}
	if len(mock.purchases) != 0 {
		t.Fatalf("non-completed transaction events credited: %d", len(mock.purchases))
	}
}

// PV7: Paddle Billing totals live at data.details.totals; the top-level
// block is only a fallback.
func TestPaddle_DetailsTotals(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	paddleSend(t, a, paddleEnvelope("transaction.completed", "evt_dt1", map[string]any{
		"id": "txn_dt1", "currency_code": "USD",
		"details": map[string]any{"totals": map[string]string{"grand_total": "4900", "total": "4900"}},
	}))
	paddleSend(t, a, paddleEnvelope("transaction.completed", "evt_dt2", map[string]any{
		"id": "txn_dt2", "currency_code": "USD",
		"details": map[string]any{"totals": map[string]string{"grand_total": "7700"}},
		"totals":  map[string]string{"grand_total": "1"},
	}))
	if len(mock.purchases) != 2 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	if got := mock.purchases[0].AmountMinor; got != 4900 {
		t.Fatalf("details.totals amount=%d want 4900", got)
	}
	if got := mock.purchases[1].AmountMinor; got != 7700 {
		t.Fatalf("details.totals must win over top-level: amount=%d want 7700", got)
	}
}

// PV8: refund proration base is the SALE gross, not the commission amount.
// Real engine: 10% of 10000 = 1000 commission; a 2500 (25%) refund must
// claw 250. Against the commission base (1000) the ratio clamped to 100%
// and wiped the whole commission.
func TestPaddle_RefundProratesAgainstGross(t *testing.T) {
	engine, store, d := newRealMoney(t)
	a := billing.NewPaddleAdapter("whsec_paddle_test", engine, mockResolving(1, 1), store)
	paddleSend(t, a, paddleEnvelope("transaction.completed", "evt_g1", map[string]any{
		"id": "txn_g", "currency_code": "USD", "totals": map[string]string{"grand_total": "10000"},
	}))
	paddleSend(t, a, paddleEnvelope("adjustment.created", "evt_g2", map[string]any{
		"id": "adj_g", "transaction_id": "txn_g", "action": "refund", "status": "approved",
		"currency_code": "USD", "totals": map[string]string{"total": "-2500"},
	}))
	if _, claw := clawbackFor(t, d); claw != -250 {
		t.Fatalf("clawback=%d want -250 (25%% of the 1000 commission)", claw)
	}
}

// PV9: custom_data values that aren't strings (a number, a boolean, an
// object) no longer fail the webhook, which Paddle then retried forever
// and the sale never credited. Scalars are kept as text.
func TestPaddle_CustomDataAnyType(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	paddleSend(t, a, paddleEnvelope("transaction.completed", "evt_cd", map[string]any{
		"id": "txn_cd", "currency_code": "USD", "totals": map[string]string{"grand_total": "4900"},
		"custom_data": map[string]any{"token": "tok_abc", "coupon": 12345, "vip": true, "meta": map[string]any{"a": 1}, "gone": nil},
	}))
	if len(mock.purchases) != 1 {
		t.Fatalf("purchases=%d", len(mock.purchases))
	}
	if p := mock.purchases[0]; p.ClickToken != "tok_abc" || p.CouponCode != "12345" {
		t.Fatalf("event=%+v want token tok_abc, coupon 12345", p)
	}
}

// PV10: commission on the tax-exclusive total (total - tax), like Stripe's
// total_excluding_tax. A refund is measured the same way, so a full refund
// claws the full commission.
func TestPaddle_TaxExcluded(t *testing.T) {
	a, mock, _ := newPaddleAdapter(t)
	paddleSend(t, a, paddleEnvelope("transaction.completed", "evt_tx", map[string]any{
		"id": "txn_tx", "currency_code": "EUR",
		"details": map[string]any{"totals": map[string]string{"total": "11900", "tax": "1900", "grand_total": "11900"}},
	}))
	if got := mock.purchases[0].AmountMinor; got != 10000 {
		t.Fatalf("amount=%d want 10000 (119.00 incl. 19%% VAT)", got)
	}

	engine, store, d := newRealMoney(t)
	b := billing.NewPaddleAdapter("whsec_paddle_test", engine, mockResolving(1, 1), store)
	paddleSend(t, b, paddleEnvelope("transaction.completed", "evt_tf1", map[string]any{
		"id": "txn_tf", "currency_code": "USD",
		"details": map[string]any{"totals": map[string]string{"total": "11900", "tax": "1900"}},
	}))
	paddleSend(t, b, paddleEnvelope("adjustment.created", "evt_tf2", map[string]any{
		"id": "adj_tf", "transaction_id": "txn_tf", "action": "refund", "status": "approved",
		"currency_code": "USD", "totals": map[string]string{"total": "-11900", "tax": "-1900"},
	}))
	var amt int64
	if err := d.QueryRow(`SELECT amount FROM commissions WHERE parent_commission_id IS NULL`).Scan(&amt); err != nil {
		t.Fatal(err)
	}
	if _, claw := clawbackFor(t, d); amt != 1000 || claw != -1000 {
		t.Fatalf("commission=%d clawback=%d want 1000 and -1000 (10%% of 10000, fully refunded)", amt, claw)
	}
}
