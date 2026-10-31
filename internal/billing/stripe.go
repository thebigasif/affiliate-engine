// stripe.go — Stripe webhook adapter: HMAC verification, event mapping,
// attribution pre-resolve, and refund→commission linkage.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// ProcessPurchaser is the slice of *commissions.Engine the adapter needs.
// Declared as an interface so tests (and alternative engines) can mock it;
// *commissions.Engine satisfies it as-is.
type ProcessPurchaser interface {
	ProcessPurchase(ctx context.Context, ev commissions.Event) (int64, error)
	ProcessRefund(ctx context.Context, ev commissions.Event, originalCommissionID, refundedMinor, originalMinor int64) error
}

// AttributionResolver is the slice of *attribution.Resolver the adapter needs
// (satisfied by *attribution.Resolver).
type AttributionResolver interface {
	Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error)
}

// CustomerBinder is the optional engine capability behind subscription
// checkouts: the lifetime customer binding written without a credit.
// *commissions.Engine implements it; *screening.Engine forwards it.
type CustomerBinder interface {
	BindCustomer(ctx context.Context, customerExtID string, affiliateID, campaignID int64, method string) error
}

// DefaultSignatureTolerance is the Stripe-recommended max age of the `t=`
// timestamp before a (correctly signed) webhook is considered stale.
const DefaultSignatureTolerance = 5 * time.Minute

// StripeAdapter verifies and maps Stripe webhooks onto the commissions
// pipeline. Construct with NewStripeAdapter.
type StripeAdapter struct {
	Secret    string              // webhook signing secret (whsec_...); the whsec_ prefix is part of the secret
	Engine    ProcessPurchaser    // *commissions.Engine
	Resolver  AttributionResolver // *attribution.Resolver; nil = skip pre-resolve
	Store     Store               // commission linkage for refunds; nil = linkage disabled
	Products  ProductFilter       // AE_STRIPE_PRODUCTS; zero value = every sale counts
	now       func() time.Time    // injectable clock
	tolerance time.Duration       // 0 = DefaultSignatureTolerance; <0 = no freshness check
}

// NewStripeAdapter wires the adapter. engine must be non-nil; resolver and
// store may be nil (attribution pre-resolve / refund linkage disabled).
func NewStripeAdapter(secret string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *StripeAdapter {
	return &StripeAdapter{
		Secret:    secret,
		Engine:    engine,
		Resolver:  resolver,
		Store:     store,
		now:       time.Now,
		tolerance: DefaultSignatureTolerance,
	}
}

// --- envelope ---

type envelope struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// HandleWebhook implements Adapter. Ordering guarantees:
//
//  1. The HMAC signature is verified over the RAW request body FIRST — no
//     database write happens before this check passes (spec: replay/forgery
//     protection must precede any persistence).
//  2. The SAME raw bytes are then parsed. On Go's encoding/json
//     duplicate-key behaviour: json.Unmarshal keeps the LAST occurrence of a
//     duplicated object key. That is safe HERE because the signature is
//     computed over the exact raw bytes being parsed — an attacker cannot
//     make the signed payload and the parsed payload diverge (any edit to
//     the body, including duplicate-key tricks, breaks the MAC). Disallow-
//     UnknownFields would not help against duplicates and would break on
//     Stripe adding fields, so we parse into narrow structs instead.
//  3. Replay protection: the engine inserts (source, external_event_id)
//     into raw_events (unique) and returns ErrAlreadyProcessed on replay,
//     which we surface as a success no-op so the provider stops retrying.
func (a *StripeAdapter) HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error {
	// (a) verify FIRST — before any DB write.
	if err := a.verifySignature(rawBody, headers.Get("Stripe-Signature")); err != nil {
		return err
	}

	// (b) parse the signed raw body.
	var env envelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedEvent, err)
	}
	if env.ID == "" || env.Type == "" || len(env.Data.Object) == 0 {
		return fmt.Errorf("%w: missing id/type/data.object", ErrMalformedEvent)
	}

	switch env.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		// async_payment_succeeded is the paid transition of a session that
		// completed "unpaid" (SEPA/ACH/OXXO/...). handleCheckout credits
		// only payment_status=paid, so each session credits exactly once.
		return a.handleCheckout(ctx, env)
	case "invoice.paid":
		return a.handleInvoice(ctx, env)
	case "charge.refunded":
		return a.handleChargeRefunded(ctx, env)
	case "charge.dispute.funds_withdrawn":
		return a.handleDisputeWithdrawn(ctx, env)
	case "charge.dispute.funds_reinstated":
		// The merchant won the dispute and has the money back. The claw
		// taken at funds_withdrawn stays: the engine has no reversal for
		// a clawback (its rows and journals are append-only, and a
		// re-credit could pay a partner twice if the merchant also
		// refunds). Logged for the operator, who can credit by hand.
		var dp dispute
		_ = json.Unmarshal(env.Data.Object, &dp)
		log.Printf("billing: stripe %s: dispute %s on %s reinstated %d %s (clawback kept; credit manually if owed)",
			env.ID, dp.ID, firstNonEmpty(string(dp.PaymentIntent), string(dp.Charge)), dp.Amount, upperCurrency(dp.Currency))
		return nil
	case "invoice_payment.paid":
		return a.handleInvoicePayment(ctx, env)
	case "customer.subscription.deleted":
		// Record-only: subscription churn is an analytics/retention fact,
		// not a money event. No commission action.
		var sub subscription
		_ = json.Unmarshal(env.Data.Object, &sub)
		log.Printf("billing: stripe customer.subscription.deleted event=%s subscription=%s customer=%s (recorded, no action)",
			env.ID, sub.ID, sub.Customer)
		return nil
	default:
		log.Printf("billing: ignoring stripe event %s type=%s", env.ID, env.Type)
		return nil
	}
}

// --- signature verification ---

// verifySignature checks Stripe-Signature "t=<unix>,v1=<hex>[,v1=<hex>...]".
// HMAC-SHA256 is computed over string(t) + "." + rawBody and compared
// timing-safely (hmac.Equal) against EVERY v1 candidate (Stripe rotates
// secrets and may send several). The raw t string is used verbatim in the
// signed payload so e.g. "t=0123" cannot be re-canonicalized to "123".
func (a *StripeAdapter) verifySignature(rawBody []byte, header string) error {
	if strings.TrimSpace(a.Secret) == "" {
		return fmt.Errorf("billing: stripe signing secret not configured")
	}
	tRaw, sigs := parseSignatureHeader(header)
	if tRaw == "" || len(sigs) == 0 {
		return fmt.Errorf("%w: missing t= or v1= in Stripe-Signature", ErrInvalidSignature)
	}

	// Freshness: reject correctly-signed but stale deliveries (blocks
	// capture-replay of old bodies). tolerance < 0 disables the check.
	if a.tolerance >= 0 {
		ts, err := strconv.ParseInt(tRaw, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: non-numeric t=", ErrInvalidSignature)
		}
		age := a.now().UTC().Sub(time.Unix(ts, 0).UTC())
		if age > a.tolerance {
			return fmt.Errorf("%w: timestamp %ds old exceeds tolerance %s", ErrInvalidSignature, int64(age.Seconds()), a.tolerance)
		}
	}

	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write([]byte(tRaw))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	expected := mac.Sum(nil)

	for _, sig := range sigs {
		if hmac.Equal(sig, expected) {
			return nil
		}
	}
	return fmt.Errorf("%w: hmac mismatch", ErrInvalidSignature)
}

// parseSignatureHeader splits the header into the timestamp token and the
// decoded v1 signatures. Elements without '=' and non-hex v1 values are
// skipped; unknown schemes (v2=...) are ignored.
func parseSignatureHeader(header string) (tRaw string, sigs [][]byte) {
	for _, elem := range strings.Split(header, ",") {
		elem = strings.TrimSpace(elem)
		if elem == "" {
			continue
		}
		eq := strings.IndexByte(elem, '=')
		if eq <= 0 {
			continue
		}
		k, v := elem[:eq], elem[eq+1:]
		switch k {
		case "t":
			if tRaw == "" && isDigits(v) {
				tRaw = v
			}
		case "v1":
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	return tRaw, sigs
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// --- event objects (narrow: only fields we consume; everything else ignored) ---

type checkoutSession struct {
	ID            string            `json:"id"`
	AmountTotal   int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	Customer      string            `json:"customer"`
	CustomerEmail string            `json:"customer_email"` // prefill only; customer_details.email is what the buyer entered
	PaymentIntent string            `json:"payment_intent"`
	Created       int64             `json:"created"`
	Metadata      map[string]string `json:"metadata"`
	// Mode is "payment" | "subscription" | "setup". Subscription sessions
	// are credited by the subscription's first invoice.paid, never here.
	Mode string `json:"mode"`
	// PaymentStatus is "paid" | "unpaid" | "no_payment_required". Async
	// methods (SEPA Debit, ACH, OXXO, Boleto, ...) complete the session
	// "unpaid"; the money lands later with
	// checkout.session.async_payment_succeeded (or never).
	PaymentStatus   string `json:"payment_status"`
	CustomerDetails struct {
		Email   string `json:"email"`
		Address struct {
			Country string `json:"country"`
		} `json:"address"`
	} `json:"customer_details"`
	TotalDetails struct {
		AmountDiscount int64 `json:"amount_discount"`
		AmountTax      int64 `json:"amount_tax"`
	} `json:"total_details"`
	Discounts []struct {
		Coupon string `json:"coupon"`
	} `json:"discount"`
	// Line items carry the plan identity for per-product rates (FR-12);
	// only the first line's price id is consumed (single-plan sessions).
	// Stripe leaves them out of webhook payloads unless expanded.
	LineItems []lineItem `json:"line_items"`
	// PaymentLink is set when a Payment Link opened the session.
	PaymentLink flexID `json:"payment_link"`
	// ClientReferenceID carries an at1_ token from Payment Links, the
	// Pricing Table and the Buy Button (t.js sets it). Any other value is
	// the merchant's own id and is ignored.
	ClientReferenceID string `json:"client_reference_id"`
	// Subscription and Invoice name a subscription-mode session's
	// subscription and its first invoice.
	Subscription flexID `json:"subscription"`
	Invoice      flexID `json:"invoice"`
}

// flexID is a Stripe id that arrives as a string, or as an object with an
// "id" when the field was expanded.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	switch {
	case string(b) == "null":
		*f = ""
		return nil
	case len(b) > 0 && b[0] == '{':
		var o struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		*f = flexID(o.ID)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*f = flexID(s)
	return nil
}

// lineItem is a checkout or invoice line. API versions before 2025-03-31
// carry price{id, product}; later ones moved it to
// pricing.price_details{price, product}. Both are read.
type lineItem struct {
	Price struct {
		ID      string `json:"id"`
		Product flexID `json:"product"`
	} `json:"price"`
	Pricing struct {
		PriceDetails struct {
			Price   flexID `json:"price"`
			Product flexID `json:"product"`
		} `json:"price_details"`
	} `json:"pricing"`
}

func (l lineItem) priceID() string {
	return firstNonEmpty(l.Price.ID, string(l.Pricing.PriceDetails.Price))
}

func (l lineItem) productID() string {
	return firstNonEmpty(string(l.Price.Product), string(l.Pricing.PriceDetails.Product))
}

// invoice.lines.data[0].price.id is the subscription's plan identity;
// metadata override wins (merchant may pin a custom product key).
type invoice struct {
	ID         string `json:"id"`
	AmountPaid int64  `json:"amount_paid"`
	// Commissionable base: prefer tax-exclusive total when present
	// (total_excluding_tax, post-discount, pre-tax); fall back to amount_paid.
	TotalExcludingTax   int64             `json:"total_excluding_tax"`
	Currency            string            `json:"currency"`
	Customer            string            `json:"customer"`
	CustomerEmail       string            `json:"customer_email"`
	PaymentIntent       string            `json:"payment_intent"`
	BillingReason       string            `json:"billing_reason"`
	Created             int64             `json:"created"`
	Metadata            map[string]string `json:"metadata"`
	SubscriptionDetails struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription_details"`
	Subscription flexID `json:"subscription"`
	// API versions from 2025-03-31 moved subscription and
	// subscription_details under parent. Both shapes are read.
	Parent struct {
		SubscriptionDetails struct {
			Metadata     map[string]string `json:"metadata"`
			Subscription flexID            `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Lines struct {
		Data []lineItem `json:"data"`
	} `json:"lines"`
	// API versions from 2025-03-31 dropped payment_intent; the payments
	// list (when the payload includes it) names the PaymentIntent or
	// charge that paid the invoice. invoice_payment.paid covers payloads
	// without it (handleInvoicePayment).
	Payments struct {
		Data []invoicePayment `json:"data"`
	} `json:"payments"`
}

func (inv invoice) subscriptionID() string {
	return firstNonEmpty(string(inv.Subscription), string(inv.Parent.SubscriptionDetails.Subscription))
}

// paymentIntent is the PaymentIntent that paid the invoice, in either
// API shape.
func (inv invoice) paymentIntent() string {
	if inv.PaymentIntent != "" {
		return inv.PaymentIntent
	}
	for _, p := range inv.Payments.Data {
		if pi := string(p.Payment.PaymentIntent); pi != "" && p.Status != "canceled" {
			return pi
		}
	}
	return ""
}

// paymentKeys are the ids a refund of this invoice's payment can carry.
func (inv invoice) paymentKeys() []string {
	keys := []string{inv.paymentIntent()}
	for _, p := range inv.Payments.Data {
		keys = append(keys, string(p.Payment.PaymentIntent), string(p.Payment.Charge))
	}
	return compact(keys)
}

// invoicePayment is an InvoicePayment (API 2025-03-31+): the object of
// invoice_payment.paid, and an entry of invoice.payments.
type invoicePayment struct {
	ID      string `json:"id"`
	Invoice flexID `json:"invoice"`
	Status  string `json:"status"`
	Payment struct {
		Type          string `json:"type"`
		PaymentIntent flexID `json:"payment_intent"`
		Charge        flexID `json:"charge"`
	} `json:"payment"`
}

// dispute is a charge.dispute.* object. Amount is the disputed amount,
// usually the whole charge.
type dispute struct {
	ID            string `json:"id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Charge        flexID `json:"charge"`
	PaymentIntent flexID `json:"payment_intent"`
	Created       int64  `json:"created"`
}

type charge struct {
	ID             string            `json:"id"`
	Amount         int64             `json:"amount"` // original captured amount (never mutated by refunds)
	AmountRefunded int64             `json:"amount_refunded"`
	Currency       string            `json:"currency"`
	Customer       string            `json:"customer"`
	ReceiptEmail   string            `json:"receipt_email"`
	BillEmail      string            `json:"billing_details.email"` // absent on charge; kept for forward-compat
	PaymentIntent  string            `json:"payment_intent"`
	Invoice        string            `json:"invoice"`
	Created        int64             `json:"created"`
	Metadata       map[string]string `json:"metadata"`
	// refunds.data is left out of webhook payloads since API 2022-11-15;
	// amount_refunded (cumulative) is the one reliable figure.
}

type subscription struct {
	ID       string            `json:"id"`
	Customer string            `json:"customer"`
	Metadata map[string]string `json:"metadata"`
}

// metadataToken returns the attribution token: aff_token wins, token is the
// fallback alias.
func metadataToken(md map[string]string) string {
	if md == nil {
		return ""
	}
	if v := md["aff_token"]; v != "" {
		return v
	}
	return md["token"]
}

// checkoutToken returns a session's attribution token: metadata wins
// (server-side integrations), then an at1_ client_reference_id (no-code
// checkouts via t.js). Any other client_reference_id is ignored: the
// Stripe account may be shared with products that use it for their own
// ids.
func checkoutToken(cs checkoutSession) string {
	if t := metadataToken(cs.Metadata); t != "" {
		return t
	}
	if t, ok := tokens.FromClientReferenceID(cs.ClientReferenceID); ok {
		return t
	}
	return ""
}

func metadataCoupon(md map[string]string) string {
	if md == nil {
		return ""
	}
	return md["coupon"]
}

// metadataProductKey returns the per-product rate key: aff_product wins,
// product is the fallback alias (mirrors the aff_token/token convention).
func metadataProductKey(md map[string]string) string {
	if md == nil {
		return ""
	}
	if v := md["aff_product"]; v != "" {
		return v
	}
	return md["product"]
}

// firstLinePrice extracts the first line item's price id (the purchased
// plan), shared by checkout sessions and invoices.
func firstLinePrice(items []lineItem) string {
	if len(items) == 0 {
		return ""
	}
	return items[0].priceID()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// mergeMetadata overlays top onto base (top wins); nil-safe.
func mergeMetadata(base, top map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(top))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range top {
		out[k] = v
	}
	return out
}

func (a *StripeAdapter) at(sec int64) time.Time {
	if sec > 0 {
		return time.Unix(sec, 0).UTC()
	}
	return a.now().UTC()
}

func upperCurrency(c string) string { return strings.ToUpper(c) }

// --- purchase-family handlers ---

func (a *StripeAdapter) handleCheckout(ctx context.Context, env envelope) error {
	var cs checkoutSession
	if err := json.Unmarshal(env.Data.Object, &cs); err != nil {
		return fmt.Errorf("%w: checkout.session object: %v", ErrMalformedEvent, err)
	}

	// Subscription-mode sessions: the subscription's first invoice.paid
	// (billing_reason=subscription_create) is the credit. Crediting the
	// session too paid every subscription sale twice. The session's token
	// still has to reach those invoices.
	if cs.Mode == "subscription" {
		log.Printf("billing: stripe %s: subscription-mode checkout %s (credited via invoice.paid, not here)",
			env.ID, cs.ID)
		return a.attributeSubscription(ctx, env, cs)
	}
	// Only money that actually landed is commissionable. Async payment
	// methods complete the session "unpaid" and may still fail; the paid
	// state arrives as checkout.session.async_payment_succeeded.
	if cs.PaymentStatus != "paid" {
		log.Printf("billing: stripe %s: checkout %s payment_status=%q (not paid, skipped)",
			env.ID, cs.ID, cs.PaymentStatus)
		return nil
	}
	// Commissionable base: after discount, before tax (amount_total is
	// what the buyer paid, tax included), like invoices' total_excluding_tax.
	amount := exTax(cs.AmountTotal, cs.TotalDetails.AmountTax)
	if amount <= 0 {
		return nil // zero-total (100% discount): not commissionable
	}
	if a.Products.Active() {
		ok, err := a.checkoutAllowed(ctx, cs)
		if err != nil {
			return err // transient (Stripe API): error so Stripe retries
		}
		if !ok {
			log.Printf("billing: stripe %s: checkout %s is not a listed product (AE_STRIPE_PRODUCTS), skipped", env.ID, cs.ID)
			return nil
		}
	}

	// Discount mapping: our affiliate coupon code rides in metadata.coupon
	// (attached at session-creation time). A Stripe-side discount id
	// (total_details.amount_discount / discounts[].coupon) is a Stripe
	// coupon object id, NOT an affiliate code — its presence is logged
	// only, so manual coupons never mis-attribute.
	coupon := metadataCoupon(cs.Metadata)
	discounted := cs.TotalDetails.AmountDiscount > 0 || len(cs.Discounts) > 0
	if discounted && coupon == "" {
		log.Printf("billing: stripe %s: discount present but no metadata.coupon (no affiliate coupon mapping)",
			env.ID)
	}

	ev := commissions.Event{
		Source:        "stripe",
		ExternalID:    env.ID, // envelope id = idempotency key in raw_events
		EventType:     "purchase",
		AmountMinor:   amount, // Stripe sends minor units already
		Currency:      upperCurrency(cs.Currency),
		CustomerExtID: cs.Customer,
		// The buyer's entered email lives in customer_details;
		// customer_email is only the (often empty) prefill.
		CustomerEmail: firstNonEmpty(cs.CustomerDetails.Email, cs.CustomerEmail),
		Country:       strings.ToUpper(cs.CustomerDetails.Address.Country),
		ClickToken:    checkoutToken(cs),
		CouponCode:    coupon,
		// Per-product rates (FR-12): explicit metadata pin wins; else the
		// session's first line-item price id (the plan purchased).
		ProductKey: firstNonEmpty(metadataProductKey(cs.Metadata), firstLinePrice(cs.LineItems)),
		CampaignID: 1, // default campaign; resolver may override
		OccurredAt: a.at(cs.Created),
		// Postback {order_id}: the payment intent is the merchant's most
		// stable order reference; the session id covers pi-less flows.
		OrderID: firstNonEmpty(cs.PaymentIntent, cs.ID),
	}
	if a.skipForAttribution(ctx, &ev) {
		return nil
	}
	// A payment-mode session with invoice_creation also sends invoice.paid
	// (billing_reason=manual) for the same PaymentIntent: whichever of the
	// two lands first credits, the other finds it linked and stops.
	unlock := lockPayment(firstNonEmpty(string(cs.Invoice), cs.PaymentIntent))
	defer unlock()
	if done, err := a.paymentCredited(ctx, env.ID, cs.PaymentIntent, string(cs.Invoice)); done || err != nil {
		return err
	}
	_, err := a.credit(ctx, ev, cs.AmountTotal, cs.ID, cs.PaymentIntent, string(cs.Invoice))
	return err
}

// paymentLocks serialize the credit of one payment across the two events
// that can carry it (a Checkout session and its invoice). A pool apart
// from moneylock: the engine takes moneylock inside, and sharing a stripe
// with it would deadlock.
var paymentLocks [256]sync.Mutex

func lockPayment(name string) func() {
	if name == "" {
		return func() {}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	m := &paymentLocks[h.Sum32()%uint32(len(paymentLocks))]
	m.Lock()
	return m.Unlock
}

// paymentCredited reports whether another event already credited this
// payment (its PaymentIntent or invoice is linked to a commission). A
// replay of the crediting event lands here too and is equally done.
func (a *StripeAdapter) paymentCredited(ctx context.Context, eventID string, keys ...string) (bool, error) {
	if a.Store == nil || len(compact(keys)) == 0 {
		return false, nil
	}
	cid, found, err := a.Store.FindCommission(ctx, "stripe", keys...)
	if err != nil {
		return false, fmt.Errorf("billing: stripe %s: payment lookup: %w", eventID, err)
	}
	if found {
		log.Printf("billing: stripe %s: payment %v already credited (commission %d), skipped", eventID, compact(keys), cid)
	}
	return found, nil
}

func (a *StripeAdapter) handleInvoice(ctx context.Context, env envelope) error {
	var inv invoice
	if err := json.Unmarshal(env.Data.Object, &inv); err != nil {
		return fmt.Errorf("%w: invoice object: %v", ErrMalformedEvent, err)
	}

	eventType := "purchase"
	if inv.BillingReason == "subscription_cycle" {
		eventType = "renewal"
	}

	amount := inv.TotalExcludingTax
	if amount <= 0 {
		amount = inv.AmountPaid
	}
	if amount <= 0 {
		return nil // $0 invoice (trial, 100% discount): not commissionable
	}
	if a.Products.Active() && !a.Products.matchLines(inv.Lines.Data) {
		log.Printf("billing: stripe %s: invoice %s is not a listed product (AE_STRIPE_PRODUCTS), skipped", env.ID, inv.ID)
		return nil
	}

	// Subscription metadata is where recurring tokens usually live
	// (subscription_details.metadata, or parent.subscription_details on
	// newer API versions); invoice-level metadata wins on conflict.
	md := mergeMetadata(mergeMetadata(inv.Parent.SubscriptionDetails.Metadata, inv.SubscriptionDetails.Metadata), inv.Metadata)

	ev := commissions.Event{
		Source:        "stripe",
		ExternalID:    env.ID,
		EventType:     eventType,
		AmountMinor:   amount,
		Currency:      upperCurrency(inv.Currency),
		CustomerExtID: inv.Customer,
		CustomerEmail: inv.CustomerEmail,
		ClickToken:    metadataToken(md),
		CouponCode:    metadataCoupon(md),
		// Per-product rates (FR-12): metadata pin > first invoice line's
		// price id (the subscription's plan).
		ProductKey: firstNonEmpty(metadataProductKey(md), firstLinePrice(inv.Lines.Data)),
		CampaignID: 1,
		OccurredAt: a.at(inv.Created),
		// Postback {order_id}: payment intent > invoice id.
		OrderID: firstNonEmpty(inv.paymentIntent(), inv.ID),
	}
	if a.skipForAttribution(ctx, &ev) {
		return nil
	}
	// Refunds find the commission by pi_/ch_ (or in_ before 2025-03-31).
	linkKeys := append([]string{inv.ID}, inv.paymentKeys()...)
	unlock := lockPayment(inv.ID)
	defer unlock()
	// The invoice of a payment-mode Checkout (invoice_creation) is the
	// same money the session credited.
	if done, err := a.paymentCredited(ctx, env.ID, inv.ID, inv.paymentIntent()); done || err != nil {
		return err
	}
	unattributed, err := a.credit(ctx, ev, inv.AmountPaid, linkKeys...)
	if err == nil && unattributed && inv.BillingReason == "subscription_create" {
		a.parkFirstInvoice(ctx, ev, inv.AmountPaid, linkKeys, inv.ID, inv.subscriptionID())
	}
	return err
}

// handleInvoicePayment records which PaymentIntent / charge paid an
// invoice (API 2025-03-31+, where neither names the other). A refund
// arrives with the pi_/ch_ only; the alias leads it to the invoice's
// commission whichever of this event and invoice.paid came first.
func (a *StripeAdapter) handleInvoicePayment(ctx context.Context, env envelope) error {
	var ip invoicePayment
	if err := json.Unmarshal(env.Data.Object, &ip); err != nil {
		return fmt.Errorf("%w: invoice_payment object: %v", ErrMalformedEvent, err)
	}
	s := sqlStore(a.Store)
	if ip.Invoice == "" || s == nil {
		return nil
	}
	if err := s.linkAlias(ctx, "stripe", string(ip.Invoice), string(ip.Payment.PaymentIntent), string(ip.Payment.Charge)); err != nil {
		return fmt.Errorf("billing: stripe %s: %w", env.ID, err) // transient (DB): Stripe retries
	}
	return nil
}

// attributeSubscription carries a subscription checkout's token to its
// invoices. The session credits nothing (its first invoice.paid does) and
// its token reaches no invoice: client_reference_id stays on the session,
// and session metadata is not copied to the subscription. So a valid
// token binds the Stripe customer to the affiliate here: the lifetime
// binding a credited purchase writes (first touch wins). Every invoice
// then resolves through it and is fraud-screened as it credits; a trial's
// first paid invoice included. A first invoice that arrived before this
// event was parked unattributed and is credited here, late.
func (a *StripeAdapter) attributeSubscription(ctx context.Context, env envelope, cs checkoutSession) error {
	tok := checkoutToken(cs)
	binder, ok := a.Engine.(CustomerBinder)
	if tok == "" || cs.Customer == "" || a.Resolver == nil || !ok {
		return nil
	}
	if a.Products.Active() {
		allowed, err := a.checkoutAllowed(ctx, cs)
		if err != nil {
			return err // transient (Stripe API): error so Stripe retries
		}
		if !allowed {
			log.Printf("billing: stripe %s: subscription checkout %s is not a listed product (AE_STRIPE_PRODUCTS), not bound", env.ID, cs.ID)
			return nil
		}
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{Token: tok, CampaignID: 1, Now: a.at(cs.Created)})
	if err != nil {
		return fmt.Errorf("billing: stripe %s: resolve checkout token: %w", env.ID, err)
	}
	if res.Method != "token" {
		log.Printf("billing: stripe %s: subscription checkout %s: token invalid, expired or affiliate not active (not bound)", env.ID, cs.ID)
		return nil
	}
	if err := binder.BindCustomer(ctx, cs.Customer, res.AffiliateID, res.CampaignID, "token"); err != nil {
		return fmt.Errorf("billing: stripe %s: %w", env.ID, err)
	}

	ps, ok := a.Store.(parkStore)
	if !ok {
		return nil
	}
	p, found, err := ps.findParked(ctx, "stripe", string(cs.Invoice), string(cs.Subscription))
	if err != nil || !found {
		return err
	}
	if p.Event.CustomerExtID != cs.Customer {
		log.Printf("billing: stripe %s: parked invoice %s belongs to another customer, not credited", env.ID, p.Event.ExternalID)
		return nil
	}
	return a.creditLate(ctx, p.Event, p.Basis, p.LinkKeys)
}

// parkFirstInvoice keeps an unattributed first invoice for its checkout.
// Stripe does not order webhooks, and invoice.paid often lands before the
// checkout.session.completed that carries the buyer's token. The invoice
// is already recorded unattributed under its own event id, so from here
// it can only credit under lateKey. The checkout and the re-check below
// share that key: raw_events lets exactly one of them credit, whichever
// way the race goes.
func (a *StripeAdapter) parkFirstInvoice(ctx context.Context, ev commissions.Event, basis int64, linkKeys []string, lookupKeys ...string) {
	ps, ok := a.Store.(parkStore)
	if !ok || a.Resolver == nil {
		return
	}
	if err := ps.park(ctx, ev.Source, parked{Event: ev, Basis: basis, LinkKeys: compact(linkKeys)}, lookupKeys...); err != nil {
		log.Printf("billing: %s/%s: %v (a later checkout cannot credit it)", ev.Source, ev.ExternalID, err)
		return
	}
	// A checkout that bound the customer while this invoice resolved found
	// nothing parked yet: look once more now that it is.
	if err := a.creditLate(ctx, ev, basis, compact(linkKeys)); err != nil {
		log.Printf("billing: %s/%s late credit re-check: %v", ev.Source, ev.ExternalID, err)
	}
}

// lateKey is the idempotency key of a parked purchase's late credit
// (provider event ids never contain ':').
func lateKey(eventID string) string { return "late:" + eventID }

// creditLate credits a parked purchase once the ladder resolves it. The
// engine is not called while it still resolves to nobody: an unattributed
// attempt would record lateKey and block the real credit later.
func (a *StripeAdapter) creditLate(ctx context.Context, ev commissions.Event, basis int64, linkKeys []string) error {
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		Token:         ev.ClickToken,
		CustomerExtID: ev.CustomerExtID,
		CouponCode:    ev.CouponCode,
		CampaignID:    ev.CampaignID,
		Now:           ev.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("billing: late attribution %s: %w", ev.ExternalID, err)
	}
	if res.Method == "" || res.Flagged {
		return nil
	}
	ev.ExternalID = lateKey(ev.ExternalID)
	ev.AffiliateID = res.AffiliateID
	ev.Method = res.Method
	if res.CampaignID != 0 {
		ev.CampaignID = res.CampaignID
	}
	if res.ClickID != "" {
		ev.ClickID = res.ClickID
	}
	_, err = a.credit(ctx, ev, basis, linkKeys...)
	return err
}

// skipForAttribution runs the attribution ladder BEFORE ProcessPurchase and
// reports whether processing should be skipped. On a clean resolution it
// stamps AffiliateID (+CampaignID when known) onto the event; on a flagged
// resolution (ip_fallback heuristic) it conservatively skips; on resolver
// error it proceeds (the engine re-resolves coupon/customer itself).
func (a *StripeAdapter) skipForAttribution(ctx context.Context, ev *commissions.Event) bool {
	if a.Resolver == nil {
		return false
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		Token:         ev.ClickToken,
		CustomerExtID: ev.CustomerExtID,
		CouponCode:    ev.CouponCode,
		IPHash:        nil, // webhooks carry no client IP; ladder stops at token
		CampaignID:    ev.CampaignID,
		Now:           ev.OccurredAt,
	})
	if err != nil {
		log.Printf("billing: attribution resolve failed for %s/%s (proceeding, engine re-resolves): %v",
			ev.Source, ev.ExternalID, err)
		return false
	}
	if res.Flagged {
		// Conservative default: a flagged heuristic never auto-credits.
		log.Printf("billing: %s/%s attribution flagged (method=%s) — skipping, manual review",
			ev.Source, ev.ExternalID, res.Method)
		return true
	}
	if res.Method != "" {
		ev.AffiliateID = res.AffiliateID
		ev.Method = res.Method
		if res.CampaignID != 0 {
			ev.CampaignID = res.CampaignID
		}
		// Click linkage (postback {click_id}): the ladder's token rung
		// identifies the exact originating click; lower rungs resolve the
		// affiliate without a click reference ("" = unknown, macro empty).
		if res.ClickID != "" {
			ev.ClickID = res.ClickID
		}
	}
	return false
}

// credit drives the engine and links the credit for refunds. basis is the
// amount charged (tax included), which refunds and disputes prorate
// against. unattributed reports that the engine recorded the event as
// unattributed (ACKed, never retried).
func (a *StripeAdapter) credit(ctx context.Context, ev commissions.Event, basis int64, linkKeys ...string) (unattributed bool, err error) {
	if a.Engine == nil {
		return false, fmt.Errorf("billing: engine not configured")
	}
	cid, err := a.Engine.ProcessPurchase(ctx, ev)
	switch {
	case err == nil:
		// fall through to linkage
	case errors.Is(err, commissions.ErrAlreadyProcessed):
		// Replay: already recorded. An earlier delivery may have credited
		// and failed to link; finish that so refunds can find it.
		return false, relinkReplay(ctx, a.Engine, a.Store, ev, basis, linkKeys...)
	case errors.Is(err, commissions.ErrNoAttribution):
		log.Printf("billing: %s/%s unattributed (recorded in raw_events, skipped)", ev.Source, ev.ExternalID)
		return true, nil // permanent condition: ACK so Stripe stops retrying
	default:
		return false, fmt.Errorf("billing: process purchase %s: %w", ev.ExternalID, err)
	}

	// Refund linkage AFTER the credit exists: cs_/in_/pi_/ch_ aliases let
	// a later charge.refunded or dispute find this commission. A failure
	// errors the webhook; Stripe's retry replays and relinks.
	return false, linkCommission(ctx, a.Store, ev, cid, basis, linkKeys...)
}

// findCommission looks the commission up by the refunded payment's ids,
// then through an invoice_payment alias (pi_/ch_ -> in_, API 2025-03-31+).
func (a *StripeAdapter) findCommission(ctx context.Context, keys ...string) (int64, bool, error) {
	id, found, err := a.Store.FindCommission(ctx, "stripe", keys...)
	if err != nil || found {
		return id, found, err
	}
	s := sqlStore(a.Store)
	if s == nil {
		return 0, false, nil
	}
	inv, ok, err := s.findAlias(ctx, "stripe", keys...)
	if err != nil || !ok {
		return 0, false, err
	}
	return a.Store.FindCommission(ctx, "stripe", inv)
}

// --- refund and dispute handlers ---

// handleChargeRefunded claws back the refunded share of a charge. Since
// API 2022-11-15 the charge's refunds list is not in webhook payloads, so
// the only amount is amount_refunded: the CUMULATIVE total over every
// refund of the charge. It goes to the engine's cumulative entry point,
// which claws only what earlier refunds have not (two $30 refunds on $100
// claw 60%, and a late or replayed older delivery claws nothing).
func (a *StripeAdapter) handleChargeRefunded(ctx context.Context, env envelope) error {
	var ch charge
	if err := json.Unmarshal(env.Data.Object, &ch); err != nil {
		return fmt.Errorf("%w: charge object: %v", ErrMalformedEvent, err)
	}
	ev := commissions.Event{
		Source:        "stripe",
		ExternalID:    env.ID, // refund event id: each refund replays-safe
		EventType:     "refund",
		AmountMinor:   ch.AmountRefunded,
		Currency:      upperCurrency(ch.Currency),
		CustomerExtID: ch.Customer,
		CustomerEmail: ch.ReceiptEmail,
		CouponCode:    metadataCoupon(ch.Metadata),
		CampaignID:    1,
		OccurredAt:    a.at(ch.Created),
	}
	// Find the parent commission credited for this charge, via any alias:
	// payment_intent (checkout/invoice linked it), the invoice id (API
	// versions before 2025-03-31), or the charge id.
	keys := compact([]string{ch.PaymentIntent, ch.Invoice, ch.ID})
	origID, found, err := a.refundParent(ctx, env.ID, keys, ch.AmountRefunded, ev.Currency)
	if err != nil || !found {
		return err
	}
	// ratio = cumulative refunded / original captured amount (both tax
	// included, whatever base the commission was credited on).
	if cr, ok := a.Engine.(CumulativeRefunder); ok {
		err = cr.ProcessRefundCumulative(ctx, ev, origID, ch.AmountRefunded, ch.Amount)
	} else {
		err = a.Engine.ProcessRefund(ctx, ev, origID, ch.AmountRefunded, ch.Amount)
	}
	if err != nil && !errors.Is(err, commissions.ErrAlreadyProcessed) {
		return fmt.Errorf("billing: process refund %s: %w", env.ID, err)
	}
	return nil // a replayed refund event is a success no-op
}

// handleDisputeWithdrawn claws back a disputed charge when Stripe takes
// the money from the merchant (Paddle's chargeback adjustment, mirrored).
// The disputed amount comes on top of what was refunded before (a charge
// cannot be refunded while disputed), so it goes in as an increment. One
// clawback per dispute: a duplicate delivery dedupes on the dispute id.
func (a *StripeAdapter) handleDisputeWithdrawn(ctx context.Context, env envelope) error {
	var dp dispute
	if err := json.Unmarshal(env.Data.Object, &dp); err != nil {
		return fmt.Errorf("%w: dispute object: %v", ErrMalformedEvent, err)
	}
	if dp.Amount <= 0 {
		return nil
	}
	key := env.ID
	if dp.ID != "" {
		key = "dispute:" + dp.ID
	}
	ev := commissions.Event{
		Source:      "stripe",
		ExternalID:  key,
		EventType:   "refund",
		AmountMinor: dp.Amount,
		Currency:    upperCurrency(dp.Currency),
		CampaignID:  1,
		OccurredAt:  a.at(dp.Created),
	}
	keys := compact([]string{string(dp.PaymentIntent), string(dp.Charge)})
	origID, found, err := a.refundParent(ctx, key, keys, dp.Amount, ev.Currency)
	if err != nil || !found {
		return err
	}
	// The dispute names no charge amount: prorate against the amount
	// charged recorded at credit time (refundBase), so it is measured like
	// charge.refunded. An older commission without one falls back to its
	// gross; a disputed amount above it clamps to a full clawback.
	original := refundBase(ctx, a.Store, origID, dp.Amount)
	err = a.Engine.ProcessRefund(ctx, ev, origID, dp.Amount, original)
	if err != nil && !errors.Is(err, commissions.ErrAlreadyProcessed) {
		return fmt.Errorf("billing: process dispute %s: %w", env.ID, err)
	}
	return nil
}

// refundParent finds the commission a refund or dispute claws. Not found
// is ACKed (and recorded for reconciliation): retrying cannot help.
func (a *StripeAdapter) refundParent(ctx context.Context, eventID string, keys []string, amount int64, currency string) (int64, bool, error) {
	if a.Store == nil || len(keys) == 0 {
		log.Printf("billing: refund %s has no linkage store/keys — recording skip", eventID)
		return 0, false, nil
	}
	origID, found, err := a.findCommission(ctx, keys...)
	if err != nil {
		// Store failure is transient (DB): error out so Stripe retries
		// before anything is written.
		return 0, false, fmt.Errorf("billing: refund %s parent lookup: %w", eventID, err)
	}
	if found && origID > 0 {
		return origID, true, nil
	}
	// Not a commissionable sale (or pre-linkage history).
	if a.Products.Active() {
		// Most unlinked refunds on a filtered account are other
		// products' sales: recording them would bury real misses.
		log.Printf("billing: refund %s: no commission linked to %v (product filter on, not recorded)", eventID, keys)
		return 0, false, nil
	}
	log.Printf("billing: refund %s: no commission linked to %v (recorded, skipped)", eventID, keys)
	if rerr := a.Store.RecordRefundMiss(ctx, "stripe", eventID, keys, amount, currency); rerr != nil {
		log.Printf("billing: record refund miss %s: %v", eventID, rerr)
	}
	return 0, false, nil
}

func compact(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Compile-time proof the concrete collaborators satisfy the interfaces the
// adapter depends on.
var (
	_ ProcessPurchaser    = (*commissions.Engine)(nil)
	_ AttributionResolver = (*attribution.Resolver)(nil)
	_ Adapter             = (*StripeAdapter)(nil)
)
