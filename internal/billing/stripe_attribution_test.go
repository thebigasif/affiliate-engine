package billing_test

// client_reference_id as a token channel, and subscription checkouts
// reaching their invoices: real engine (behind fraud screening, as main
// wires it), real resolver + signer, real SQLStore.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/screening"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

type attrEnv struct {
	h      *db.Handles
	a      *billing.StripeAdapter
	signer *tokens.Signer
}

func newAttrEnv(t *testing.T) *attrEnv {
	t.Helper()
	store, h := newSQLStore(t)
	ctx := context.Background()
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability", "AFFILIATE_PAYABLE:2:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev'), (2, 'b@t.dev')`,
		`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		 VALUES (1, NULL, 'purchase', 'percent', 1000)`, // 10%
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	secret, _ := tokens.NewRandomSecret(32)
	signer, _ := tokens.New(secret)
	engine := screening.New(commissions.NewEngine(h.DB, p), fraud.NewEngine(h.DB), false)
	a := billing.NewStripeAdapter(testSecret, engine, attribution.NewResolver(h.DB, signer), store)
	return &attrEnv{h: h, a: a, signer: signer}
}

// click records a click + attribution row for aff (what /r/ writes) and
// returns its signed token.
func (e *attrEnv) click(t *testing.T, id string, aff int64) string {
	t.Helper()
	now := time.Now().UTC()
	if _, err := e.h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
		VALUES (?, ?, 20260929, 202609, ?, 1)`, id, now.Format(time.RFC3339Nano), aff); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
		VALUES (?, ?, 1, 'token', ?, ?)`, id, aff,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(30*24*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	tok, err := e.signer.Sign(id, aff, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *attrEnv) send(t *testing.T, body []byte) {
	t.Helper()
	if err := e.a.HandleWebhook(context.Background(), body, signedHeaders(body)); err != nil {
		t.Fatalf("webhook: %v", err)
	}
}

type creditRow struct {
	aff, amount int64
	clickID     string
}

// credits lists positive commissions (clawbacks excluded).
func (e *attrEnv) credits(t *testing.T) []creditRow {
	t.Helper()
	rows, err := e.h.DB.Query(`SELECT affiliate_id, amount, COALESCE(click_id, '') FROM commissions WHERE amount > 0 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []creditRow
	for rows.Next() {
		var c creditRow
		if err := rows.Scan(&c.aff, &c.amount, &c.clickID); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (e *attrEnv) boundTo(t *testing.T, customer string) int64 {
	t.Helper()
	var aff int64
	_ = e.h.DB.QueryRow(`SELECT affiliate_id FROM customers WHERE external_customer_id = ?`, customer).Scan(&aff)
	return aff
}

type obj = map[string]any

func stripeEvent(id, typ string, o obj) []byte {
	b, _ := json.Marshal(obj{"id": id, "type": typ, "data": obj{"object": o}})
	return b
}

func session(id, mode, customer, clientRef string, extra obj) obj {
	o := obj{
		"id": id, "object": "checkout.session", "mode": mode, "payment_status": "paid",
		"amount_total": 5000, "currency": "usd", "customer": customer,
		"customer_details": obj{"email": "buyer@shop.test"},
		"created":          time.Now().Unix(),
	}
	if clientRef != "" {
		o["client_reference_id"] = clientRef
	}
	if mode == "subscription" {
		o["subscription"] = "sub_" + customer
		o["invoice"] = "in_first_" + customer
	} else {
		o["payment_intent"] = "pi_" + id
	}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

func firstInvoice(customer string, extra obj) obj {
	o := obj{
		"id": "in_first_" + customer, "amount_paid": 5000, "total_excluding_tax": 5000,
		"currency": "usd", "customer": customer, "customer_email": "buyer@shop.test",
		"payment_intent": "pi_first_" + customer, "billing_reason": "subscription_create",
		"subscription": "sub_" + customer, "created": time.Now().Unix(),
	}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

func ref(t *testing.T, tok string) string {
	t.Helper()
	r, ok := tokens.ClientReferenceID(tok)
	if !ok {
		t.Fatal("encode client_reference_id")
	}
	return r
}

// Payment mode: a Payment Link opened with client_reference_id=at1_...
// credits the token's affiliate, with the originating click linked.
func TestClientRef_PaymentModeCredited(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_pay", 2)
	body := stripeEvent("evt_pay_1", "checkout.session.completed", session("cs_pay", "payment", "cus_pay", ref(t, tok), nil))
	e.send(t, body)
	e.send(t, body) // replay
	got := e.credits(t)
	if len(got) != 1 || got[0].aff != 2 || got[0].amount != 500 || got[0].clickID != "clk_pay" {
		t.Fatalf("credits=%+v, want one 500 credit for affiliate 2 via clk_pay", got)
	}
}

// Subscription mode, checkout first: the session binds the customer, the
// first invoice credits through the binding exactly once, renewals follow.
func TestClientRef_SubscriptionCheckoutThenInvoice(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_sub", 2)
	cs := stripeEvent("evt_cs_sub", "checkout.session.completed", session("cs_sub", "subscription", "cus_s", ref(t, tok), nil))
	e.send(t, cs)
	if n := len(e.credits(t)); n != 0 {
		t.Fatalf("subscription checkout credited %d (the invoice does)", n)
	}
	if aff := e.boundTo(t, "cus_s"); aff != 2 {
		t.Fatalf("customer bound to %d, want 2", aff)
	}

	inv := stripeEvent("evt_inv_first", "invoice.paid", firstInvoice("cus_s", nil))
	e.send(t, inv)
	e.send(t, inv) // replay
	e.send(t, cs)  // late replay of the checkout
	got := e.credits(t)
	if len(got) != 1 || got[0].aff != 2 || got[0].amount != 500 {
		t.Fatalf("credits=%+v, want exactly one 500 credit for affiliate 2", got)
	}

	renewal := firstInvoice("cus_s", obj{"id": "in_2", "billing_reason": "subscription_cycle", "payment_intent": "pi_2"})
	e.send(t, stripeEvent("evt_inv_renew", "invoice.paid", renewal))
	if got := e.credits(t); len(got) != 2 || got[1].aff != 2 {
		t.Fatalf("renewal: credits=%+v", got)
	}
	var parkedRows int
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE external_event_id LIKE 'parked:%'`).Scan(&parkedRows)
	if parkedRows != 0 {
		t.Fatalf("attributed invoice was parked (%d rows)", parkedRows)
	}
}

// Subscription mode, invoice first (Stripe does not order webhooks): the
// unattributed first invoice is parked and the checkout credits it late,
// exactly once, refundable like any other credit.
func TestClientRef_InvoiceBeforeCheckout(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_late", 2)
	inv := stripeEvent("evt_inv_early", "invoice.paid", firstInvoice("cus_l", nil))
	e.send(t, inv)
	if n := len(e.credits(t)); n != 0 {
		t.Fatalf("unattributed invoice credited %d", n)
	}

	cs := session("cs_late", "subscription", "cus_l", ref(t, tok), nil)
	e.send(t, stripeEvent("evt_cs_late", "checkout.session.completed", cs))
	got := e.credits(t)
	if len(got) != 1 || got[0].aff != 2 || got[0].amount != 500 {
		t.Fatalf("late credit: %+v, want one 500 credit for affiliate 2", got)
	}

	// Every replay and the async-success twin stay no-ops.
	e.send(t, inv)
	e.send(t, stripeEvent("evt_cs_late", "checkout.session.completed", cs))
	e.send(t, stripeEvent("evt_cs_late_async", "checkout.session.async_payment_succeeded", cs))
	if got := e.credits(t); len(got) != 1 {
		t.Fatalf("credits after replays = %d, want 1", len(got))
	}

	// The late credit links for refunds like a normal one.
	refund := stripeEvent("evt_rf_late", "charge.refunded", obj{
		"id": "ch_late", "amount": 5000, "amount_refunded": 5000, "currency": "usd",
		"customer": "cus_l", "payment_intent": "pi_first_cus_l", "invoice": "in_first_cus_l",
		"created": time.Now().Unix(),
	})
	e.send(t, refund)
	var claw int64
	if err := e.h.DB.QueryRow(`SELECT amount FROM commissions WHERE amount < 0`).Scan(&claw); err != nil || claw != -500 {
		t.Fatalf("clawback=%d err=%v, want -500", claw, err)
	}
}

// Both orders, delivered concurrently: exactly one credit every time.
func TestClientRef_ConcurrentDeliveryExactlyOnce(t *testing.T) {
	for i := 0; i < 8; i++ {
		e := newAttrEnv(t)
		tok := e.click(t, fmt.Sprintf("clk_race_%d", i), 2)
		inv := stripeEvent("evt_inv_race", "invoice.paid", firstInvoice("cus_r", nil))
		cs := stripeEvent("evt_cs_race", "checkout.session.completed", session("cs_race", "subscription", "cus_r", ref(t, tok), nil))
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, body := range [][]byte{inv, cs} {
			wg.Add(1)
			go func(b []byte) {
				defer wg.Done()
				errs <- e.a.HandleWebhook(context.Background(), b, signedHeaders(b))
			}(body)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		if got := e.credits(t); len(got) != 1 || got[0].aff != 2 {
			t.Fatalf("round %d: credits=%+v, want exactly one for affiliate 2", i, got)
		}
	}
}

// A client_reference_id that is not ours (the account's other products
// use the field) is ignored: no credit, no binding, no error.
func TestClientRef_ForeignIgnored(t *testing.T) {
	e := newAttrEnv(t)
	for i, foreign := range []string{"user_42", "at1_nottoken", "at1__"} {
		e.send(t, stripeEvent(fmt.Sprintf("evt_f_pay_%d", i), "checkout.session.completed",
			session(fmt.Sprintf("cs_f_%d", i), "payment", "cus_f", foreign, nil)))
		e.send(t, stripeEvent(fmt.Sprintf("evt_f_sub_%d", i), "checkout.session.completed",
			session(fmt.Sprintf("cs_fs_%d", i), "subscription", "cus_fs", foreign, nil)))
	}
	if got := e.credits(t); len(got) != 0 {
		t.Fatalf("foreign client_reference_id credited: %+v", got)
	}
	if aff := e.boundTo(t, "cus_fs"); aff != 0 {
		t.Fatalf("foreign client_reference_id bound the customer to %d", aff)
	}
}

// A tampered at1_ value decodes but fails the HMAC: ignored on both paths.
func TestClientRef_TamperedIgnored(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_tamper", 2)
	r := []byte(ref(t, tok))
	i := len(tokens.ClientReferencePrefix) + 5
	if r[i] == 'A' {
		r[i] = 'B'
	} else {
		r[i] = 'A'
	}
	e.send(t, stripeEvent("evt_t_pay", "checkout.session.completed", session("cs_t", "payment", "cus_t", string(r), nil)))
	e.send(t, stripeEvent("evt_t_sub", "checkout.session.completed", session("cs_ts", "subscription", "cus_ts", string(r), nil)))
	e.send(t, stripeEvent("evt_t_inv", "invoice.paid", firstInvoice("cus_ts", nil)))
	if got := e.credits(t); len(got) != 0 {
		t.Fatalf("tampered token credited: %+v", got)
	}
	if aff := e.boundTo(t, "cus_ts"); aff != 0 {
		t.Fatalf("tampered token bound the customer to %d", aff)
	}
}

// Server-side integrations: metadata.aff_token on a subscription-mode
// session used to reach nothing (the session is skipped, and its metadata
// is not copied to the subscription). It now binds like
// client_reference_id. subscription_data.metadata reaches the invoice
// directly, in both the old and the 2025-03-31+ (parent.*) shape.
func TestSubscriptionCheckout_MetadataTokens(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_md", 2)
	e.send(t, stripeEvent("evt_md_cs", "checkout.session.completed",
		session("cs_md", "subscription", "cus_md", "", obj{"metadata": obj{"aff_token": tok}})))
	if aff := e.boundTo(t, "cus_md"); aff != 2 {
		t.Fatalf("session metadata token: bound to %d, want 2", aff)
	}

	tok2 := e.click(t, "clk_sd", 1)
	e.send(t, stripeEvent("evt_sd_inv", "invoice.paid", firstInvoice("cus_sd", obj{
		"subscription_details": obj{"metadata": obj{"aff_token": tok2}},
	})))
	tok3 := e.click(t, "clk_parent", 2)
	e.send(t, stripeEvent("evt_parent_inv", "invoice.paid", firstInvoice("cus_parent", obj{
		"subscription": nil,
		"parent": obj{"type": "subscription_details", "subscription_details": obj{
			"metadata": obj{"aff_token": tok3}, "subscription": "sub_cus_parent",
		}},
	})))
	got := e.credits(t)
	if len(got) != 2 || got[0].aff != 1 || got[0].clickID != "clk_sd" || got[1].aff != 2 || got[1].clickID != "clk_parent" {
		t.Fatalf("subscription_data.metadata credits=%+v", got)
	}
}

// The late credit is screened like any other: a self-referral is held.
func TestClientRef_LateCreditScreened(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_self", 2)
	e.send(t, stripeEvent("evt_self_inv", "invoice.paid", firstInvoice("cus_self", obj{"customer_email": "B+shop@t.dev"})))
	e.send(t, stripeEvent("evt_self_cs", "checkout.session.completed", session("cs_self", "subscription", "cus_self", ref(t, tok), nil)))
	var held int
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds h JOIN commissions c ON c.id = h.commission_id
		WHERE c.affiliate_id = 2`).Scan(&held)
	if len(e.credits(t)) != 1 || held != 1 {
		t.Fatalf("credits=%d held=%d, want the late self-referral credited and held", len(e.credits(t)), held)
	}
}

// AE_STRIPE_PRODUCTS: a subscription to an unlisted product binds nothing.
func TestSubscriptionCheckout_ProductFilter(t *testing.T) {
	e := newAttrEnv(t)
	f, err := billing.ParseProductFilter("price_ours")
	if err != nil {
		t.Fatal(err)
	}
	e.a.Products = f
	tok := e.click(t, "clk_pf", 2)
	e.send(t, stripeEvent("evt_pf_other", "checkout.session.completed", session("cs_pf1", "subscription", "cus_other", ref(t, tok),
		obj{"line_items": []obj{{"price": obj{"id": "price_other", "product": "prod_other"}}}})))
	e.send(t, stripeEvent("evt_pf_ours", "checkout.session.completed", session("cs_pf2", "subscription", "cus_ours", ref(t, tok),
		obj{"line_items": []obj{{"price": obj{"id": "price_ours", "product": "prod_ours"}}}})))
	if e.boundTo(t, "cus_other") != 0 || e.boundTo(t, "cus_ours") != 2 {
		t.Fatalf("bindings: other=%d ours=%d, want 0/2", e.boundTo(t, "cus_other"), e.boundTo(t, "cus_ours"))
	}
}

// hookResolver runs a hook before the Nth Resolve call: it replays one
// exact webhook interleaving deterministically.
type hookResolver struct {
	inner billing.AttributionResolver
	calls int
	hooks map[int]func()
}

func (h *hookResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	h.calls++
	if f := h.hooks[h.calls]; f != nil {
		delete(h.hooks, h.calls)
		f()
	}
	return h.inner.Resolve(ctx, q)
}

// The tightest interleaving: the checkout lands after the invoice parked
// itself but before the invoice's own re-check. Both paths then credit
// the parked invoice; the shared late key lets exactly one through.
func TestClientRef_BothLatePathsCreditOnce(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_both", 2)
	cs := stripeEvent("evt_cs_both", "checkout.session.completed", session("cs_both", "subscription", "cus_b", ref(t, tok), nil))
	hr := &hookResolver{inner: e.a.Resolver}
	// Call 1: the invoice's ladder (unattributed). Call 2: its re-check
	// after parking; the whole checkout runs first.
	hr.hooks = map[int]func(){2: func() { e.send(t, cs) }}
	e.a.Resolver = hr
	e.send(t, stripeEvent("evt_inv_both", "invoice.paid", firstInvoice("cus_b", nil)))
	if hr.calls < 4 {
		t.Fatalf("interleaving not exercised (resolver calls=%d)", hr.calls)
	}
	if got := e.credits(t); len(got) != 1 || got[0].aff != 2 {
		t.Fatalf("credits=%+v, want exactly one for affiliate 2", got)
	}
}

// A trial: the checkout completes with nothing paid and the first invoice
// is $0. The binding carries the token to the first paid invoice, weeks
// later, whatever the click's attribution window says by then.
func TestClientRef_TrialCreditsFirstPaidInvoice(t *testing.T) {
	e := newAttrEnv(t)
	tok := e.click(t, "clk_trial", 2)
	e.send(t, stripeEvent("evt_trial_cs", "checkout.session.completed", session("cs_trial", "subscription", "cus_tr", ref(t, tok),
		obj{"payment_status": "no_payment_required", "amount_total": 0})))
	e.send(t, stripeEvent("evt_trial_inv0", "invoice.paid", firstInvoice("cus_tr", obj{"amount_paid": 0, "total_excluding_tax": 0})))
	// Window long gone by the first paid invoice.
	if _, err := e.h.DB.Exec(`UPDATE attributions SET expires_at = '2000-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	e.send(t, stripeEvent("evt_trial_inv1", "invoice.paid", firstInvoice("cus_tr", obj{
		"id": "in_after_trial", "billing_reason": "subscription_cycle", "payment_intent": "pi_after_trial",
	})))
	if got := e.credits(t); len(got) != 1 || got[0].aff != 2 || got[0].amount != 500 {
		t.Fatalf("credits=%+v, want the first paid invoice credited to affiliate 2", got)
	}
}

// coupon_hijack reads the ladder rung each commission was credited by.
// Adapters resolve the ladder themselves, so the rung has to travel with
// the event: before, every adapter credit was stored as "explicit" and
// the rule could never fire on real webhooks. Customers already referred
// by affiliate 1 buy with affiliate 2's code: the third one freezes it.
func TestCouponHijackFiresThroughTheAdapter(t *testing.T) {
	e := newAttrEnv(t)
	if _, err := e.h.DB.Exec(`INSERT INTO coupons (code, affiliate_id) VALUES ('LEAK20', 2)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		cus := fmt.Sprintf("cus_hj%d", i)
		tok := e.click(t, fmt.Sprintf("clk_hj%d", i), 1)
		// First purchase through affiliate 1's link binds the customer.
		e.send(t, stripeEvent(fmt.Sprintf("evt_hj_first_%d", i), "checkout.session.completed",
			session("cs_hj_first_"+cus, "payment", cus, "", obj{"metadata": obj{"aff_token": tok}})))
		// The repeat purchase carries affiliate 2's code.
		e.send(t, stripeEvent(fmt.Sprintf("evt_hj_coupon_%d", i), "checkout.session.completed",
			session("cs_hj_coupon_"+cus, "payment", cus, "", obj{"metadata": obj{"coupon": "LEAK20"}})))
	}
	var methods []string
	rows, err := e.h.DB.Query(`SELECT attribution_method FROM commissions WHERE amount > 0 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		methods = append(methods, m)
	}
	_ = rows.Close()
	if fmt.Sprint(methods) != "[token coupon token coupon token coupon]" {
		t.Fatalf("attribution methods = %v", methods)
	}
	var verdicts int
	if err := e.h.DB.QueryRow(`SELECT COUNT(*) FROM fraud_verdicts WHERE rule = 'coupon_hijack'`).Scan(&verdicts); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = e.h.DB.QueryRow(`SELECT status FROM affiliates WHERE id = 2`).Scan(&status)
	if verdicts == 0 || status != "frozen" {
		t.Fatalf("coupon_hijack verdicts=%d, coupon owner status=%q", verdicts, status)
	}
}
