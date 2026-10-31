// lemonsqueezy.go — Lemon Squeezy webhook adapter (FR-31, 4th of 5 PRD
// billing providers).
//
// Signature scheme (docs.lemonsqueezy.com/help/webhooks/signing-requests):
// the X-Signature header is a lowercase hex HMAC-SHA256 of the RAW request
// body keyed by the endpoint's signing secret. Verified timing-safe over
// the decoded bytes BEFORE any parse or DB write; replays dedupe
// downstream in commissions.Engine via raw_events.
//
// Payload: {meta:{event_name, test_mode}, data:{id, type:"orders",
// attributes:{...}}}. Money fields (total, subtotal, refunded_amount, ...)
// are INTEGER minor units (cents), same convention as Stripe/Chargebee —
// consumed as-is, never rescaled. The *_formatted siblings are display
// strings and are ignored. attributes.refunded is a BOOLEAN flag; the
// amount is refunded_amount.
//
// Attribution: the merchant passes the affiliate token at checkout as
// checkout[custom][token] (checkout_data.custom.token over the API). Lemon
// Squeezy delivers it in meta.custom_data on the order and subscription
// webhooks; attributes.custom_data is read too, as a fallback.
//
// Subscriptions: order_created credits the first payment and
// subscription_payment_success credits every later one (billing_reason
// "renewal" or "updated"); the "initial" invoice is the order's own
// payment and is skipped so it never credits twice. A $0 order (free
// trial) and subscription_created bind the customer to the affiliate
// without crediting, so the first paid renewal earns through the binding.
// subscription_payment_refunded claws back the refunded share of that
// payment.
//
// Test-mode deliveries (meta.test_mode=true) are ACKed without crediting:
// anyone with a store in test mode can mint free "orders", so they must
// never reach the money path.
//
// Lemon Squeezy payloads carry NO event id: meta.webhook_id identifies the
// endpoint, not the delivery. The idempotency key is therefore derived:
//
//	purchase     -> "order_created:<order id>"
//	refund       -> "order_refunded:<order id>:<cumulative refunded minor>"
//	renewal      -> "subscription_payment_success:<invoice id>"
//	renewal refund -> "subscription_payment_refunded:<invoice id>:<cumulative>"
//
// The refund key includes the cumulative amount so a SECOND partial refund
// on the same order (larger cumulative) is a distinct event, while replaying
// the same delivery dedupes. refunded_amount is cumulative across the
// order's lifetime, so it is handed to the engine's cumulative entry point
// (ProcessRefundCumulative), which claws only the part of the running total
// not already clawed.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// CumulativeRefunder is the optional engine capability for providers that
// report the CUMULATIVE refunded total on every refund event (Lemon
// Squeezy). *commissions.Engine and the screening decorator implement it;
// an engine without it receives the value through ProcessRefund.
type CumulativeRefunder interface {
	ProcessRefundCumulative(ctx context.Context, ev commissions.Event, originalCommissionID, cumulativeRefundedMinor, originalMinor int64) error
}

var _ CumulativeRefunder = (*commissions.Engine)(nil)

// LemonSqueezyAdapter verifies and maps Lemon Squeezy order webhooks.
type LemonSqueezyAdapter struct {
	Secret   string
	Engine   ProcessPurchaser
	Resolver AttributionResolver
	Store    Store
}

// NewLemonSqueezyAdapter builds one. engine must be non-nil; resolver and
// store follow the same nil conventions as the other adapters.
func NewLemonSqueezyAdapter(secret string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *LemonSqueezyAdapter {
	return &LemonSqueezyAdapter{Secret: secret, Engine: engine, Resolver: resolver, Store: store}
}

// lsEnvelope is the JSON:API-ish wrapper every LS webhook uses.
type lsEnvelope struct {
	Meta struct {
		EventName  string                     `json:"event_name"`
		TestMode   bool                       `json:"test_mode"`
		CustomData map[string]json.RawMessage `json:"custom_data"`
	} `json:"meta"`
	Data json.RawMessage `json:"data"`
}

type lsOrder struct {
	ID         string `json:"id"`
	Attributes struct {
		Identifier     string       `json:"identifier"` // order UUID
		OrderNumber    int64        `json:"order_number"`
		UserEmail      string       `json:"user_email"`
		CustomerID     int64        `json:"customer_id"`
		Currency       string       `json:"currency"`
		Subtotal       int64        `json:"subtotal"` // minor units, before discount and tax
		Total          int64        `json:"total"`    // minor units, tax included
		Tax            int64        `json:"tax"`      // minor units: the tax part of total
		CreatedAt      string       `json:"created_at"`
		RefundedAt     string       `json:"refunded_at"`
		UpdatedAt      string       `json:"updated_at"`
		Status         string       `json:"status"`
		Refunded       bool         `json:"refunded"`        // flag only
		RefundedAmount int64        `json:"refunded_amount"` // cumulative, minor units
		FirstOrderItem *lsOrderItem `json:"first_order_item"`
		// CustomData stays RawMessage: LS merchants can stash non-string
		// JSON values, and a type mismatch must never fail the whole
		// order (lost commission). String values are extracted by
		// lsCustom; non-strings are ignored.
		CustomData map[string]json.RawMessage `json:"custom_data"`
	} `json:"attributes"`
}

// lsCustom returns the string value of a custom_data key ("" when absent
// or non-string).
func lsCustom(md map[string]json.RawMessage, key string) string {
	raw, ok := md[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// lsCustomValue reads a checkout custom-data key where Lemon Squeezy puts
// it (meta.custom_data), falling back to the object's attributes.
func lsCustomValue(env lsEnvelope, attrs map[string]json.RawMessage, key string) string {
	if v := lsCustom(env.Meta.CustomData, key); v != "" {
		return v
	}
	return lsCustom(attrs, key)
}

// lsSubscription is the subscription_created payload (data.type
// "subscriptions").
type lsSubscription struct {
	ID         string `json:"id"`
	Attributes struct {
		CustomerID int64  `json:"customer_id"`
		UserEmail  string `json:"user_email"`
		VariantID  int64  `json:"variant_id"`
	} `json:"attributes"`
}

// lsInvoice is a subscription payment (data.type "subscription-invoices").
type lsInvoice struct {
	ID         string `json:"id"`
	Attributes struct {
		SubscriptionID int64  `json:"subscription_id"`
		CustomerID     int64  `json:"customer_id"`
		UserEmail      string `json:"user_email"`
		BillingReason  string `json:"billing_reason"` // initial | renewal | updated
		Currency       string `json:"currency"`
		Status         string `json:"status"`
		Total          int64  `json:"total"`           // minor units, tax included
		Tax            int64  `json:"tax"`             // minor units: the tax part of total
		RefundedAmount int64  `json:"refunded_amount"` // cumulative, minor units
		CreatedAt      string `json:"created_at"`
		RefundedAt     string `json:"refunded_at"`
		UpdatedAt      string `json:"updated_at"`
	} `json:"attributes"`
}

func lsInvoiceKey(id string) string { return "subinv:" + id }

type lsOrderItem struct {
	ProductID int64 `json:"product_id"`
	VariantID int64 `json:"variant_id"`
}

// HandleWebhook implements Adapter.
func (a *LemonSqueezyAdapter) HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error {
	// 1) Signature FIRST, over the exact bytes that will be parsed.
	sig := headers.Get("X-Signature")
	if sig == "" {
		return fmt.Errorf("%w: missing X-Signature", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write(rawBody)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%w: bad hex signature", ErrInvalidSignature)
	}
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
	}

	// 2) Envelope.
	var env lsEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return fmt.Errorf("%w: lemonsqueezy envelope: %v", ErrMalformedEvent, err)
	}

	// 3) Test-mode orders are free to create: ACK, never credit or claw.
	if env.Meta.TestMode {
		log.Printf("billing: lemonsqueezy %s test_mode delivery (ACK, skipped)", env.Meta.EventName)
		return nil
	}

	switch env.Meta.EventName {
	case "order_created":
		return a.handleOrderCreated(ctx, env)
	case "order_refunded":
		return a.handleOrderRefunded(ctx, env)
	case "subscription_created":
		return a.handleSubscriptionCreated(ctx, env)
	case "subscription_payment_success":
		return a.handleInvoicePaid(ctx, env)
	case "subscription_payment_refunded":
		return a.handleInvoiceRefunded(ctx, env)
	default:
		return nil // other subscription_*/customer_* etc: forward-compatible silent success
	}
}

func (a *LemonSqueezyAdapter) handleOrderCreated(ctx context.Context, env lsEnvelope) error {
	var order lsOrder
	if err := json.Unmarshal(env.Data, &order); err != nil {
		return fmt.Errorf("%w: lemonsqueezy order: %v", ErrMalformedEvent, err)
	}
	if order.Attributes.Status != "paid" {
		return nil // not yet paid: not commissionable
	}
	if order.Attributes.Total <= 0 {
		// $0 order (free trial, 100% discount): nothing to credit, but the
		// buyer came through the affiliate, so bind them for renewals.
		return a.bindFromToken(ctx, env, order.Attributes.CustomData, order.Attributes.CustomerID)
	}
	// Commission on the tax-exclusive amount (Stripe/Paddle/Polar parity);
	// already minor units.
	amount := exTax(order.Attributes.Total, order.Attributes.Tax)
	if amount <= 0 {
		return nil
	}
	ev := commissions.Event{
		Source:        "lemonsqueezy",
		ExternalID:    "order_created:" + order.ID,
		EventType:     "purchase",
		AmountMinor:   amount,
		Currency:      strings.ToUpper(order.Attributes.Currency),
		CustomerExtID: strconv.FormatInt(order.Attributes.CustomerID, 10),
		CustomerEmail: order.Attributes.UserEmail,
		ClickToken:    lsCustomValue(env, order.Attributes.CustomData, "token"),
		CouponCode:    lsCustomValue(env, order.Attributes.CustomData, "coupon"),
		// Per-product rates (FR-12): the purchased variant is the LS
		// analog of a plan price point.
		ProductKey: lsProductKey(order.Attributes.FirstOrderItem),
		CampaignID: 1,
		OccurredAt: eventTime(order.Attributes.CreatedAt),
		// Postback {order_id}: the human-facing order number.
		OrderID: strconv.FormatInt(order.Attributes.OrderNumber, 10),
	}
	return a.resolveAndPurchase(ctx, &ev, order.Attributes.Total, ev.ExternalID, order.ID, order.Attributes.Identifier)
}

// handleSubscriptionCreated binds the subscriber to the affiliate whose
// token came through checkout. No money moves; order_created (paid) or
// the renewals credit.
func (a *LemonSqueezyAdapter) handleSubscriptionCreated(ctx context.Context, env lsEnvelope) error {
	var sub lsSubscription
	if err := json.Unmarshal(env.Data, &sub); err != nil {
		return fmt.Errorf("%w: lemonsqueezy subscription: %v", ErrMalformedEvent, err)
	}
	return a.bindFromToken(ctx, env, nil, sub.Attributes.CustomerID)
}

// bindFromToken writes the lifetime customer binding from a checkout token
// without crediting (the Stripe subscription-checkout pattern). Missing
// token, customer or binder capability = nothing to do.
func (a *LemonSqueezyAdapter) bindFromToken(ctx context.Context, env lsEnvelope, attrs map[string]json.RawMessage, customerID int64) error {
	tok := lsCustomValue(env, attrs, "token")
	binder, ok := a.Engine.(CustomerBinder)
	if tok == "" || customerID <= 0 || a.Resolver == nil || !ok {
		return nil
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{Token: tok, CampaignID: 1, Now: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("lemonsqueezy: resolve checkout token: %w", err)
	}
	if res.Method != "token" {
		log.Printf("billing: lemonsqueezy %s: token invalid, expired or affiliate not active (not bound)", env.Meta.EventName)
		return nil
	}
	return binder.BindCustomer(ctx, strconv.FormatInt(customerID, 10), res.AffiliateID, res.CampaignID, "token")
}

// handleInvoicePaid credits a subscription renewal (or a plan-change
// charge). The "initial" invoice is the first order's payment, which
// order_created already credited.
func (a *LemonSqueezyAdapter) handleInvoicePaid(ctx context.Context, env lsEnvelope) error {
	var inv lsInvoice
	if err := json.Unmarshal(env.Data, &inv); err != nil {
		return fmt.Errorf("%w: lemonsqueezy invoice: %v", ErrMalformedEvent, err)
	}
	amount := exTax(inv.Attributes.Total, inv.Attributes.Tax)
	if inv.Attributes.BillingReason == "initial" || inv.Attributes.Status != "paid" || amount <= 0 {
		return nil
	}
	ev := commissions.Event{
		Source:        "lemonsqueezy",
		ExternalID:    "subscription_payment_success:" + inv.ID,
		EventType:     "purchase",
		AmountMinor:   amount,
		Currency:      strings.ToUpper(inv.Attributes.Currency),
		CustomerExtID: strconv.FormatInt(inv.Attributes.CustomerID, 10),
		CustomerEmail: inv.Attributes.UserEmail,
		ClickToken:    lsCustomValue(env, nil, "token"),
		CouponCode:    lsCustomValue(env, nil, "coupon"),
		CampaignID:    1,
		OccurredAt:    eventTime(inv.Attributes.CreatedAt),
		OrderID:       inv.ID,
	}
	return a.resolveAndPurchase(ctx, &ev, inv.Attributes.Total, ev.ExternalID, lsInvoiceKey(inv.ID))
}

// handleInvoiceRefunded claws back a refunded renewal. refunded_amount is
// cumulative, like orders. An "initial" invoice belongs to the order, whose
// order_refunded delivery claws it.
func (a *LemonSqueezyAdapter) handleInvoiceRefunded(ctx context.Context, env lsEnvelope) error {
	var inv lsInvoice
	if err := json.Unmarshal(env.Data, &inv); err != nil {
		return fmt.Errorf("%w: lemonsqueezy invoice refund: %v", ErrMalformedEvent, err)
	}
	refunded := inv.Attributes.RefundedAmount
	if inv.Attributes.BillingReason == "initial" || refunded <= 0 {
		return nil
	}
	cur := strings.ToUpper(inv.Attributes.Currency)
	refundKey := "subscription_payment_refunded:" + inv.ID + ":" + strconv.FormatInt(refunded, 10)
	return a.clawCumulative(ctx, refundKey, cur, eventTime(inv.Attributes.RefundedAt, inv.Attributes.UpdatedAt),
		refunded, inv.Attributes.Total, lsInvoiceKey(inv.ID))
}

// lsProductKey returns the variant id as the per-product rate key; "" when
// the order carries no line items (should not happen, but never block a
// commission over it).
func lsProductKey(item *lsOrderItem) string {
	if item == nil {
		return ""
	}
	return strconv.FormatInt(item.VariantID, 10)
}

func (a *LemonSqueezyAdapter) handleOrderRefunded(ctx context.Context, env lsEnvelope) error {
	var order lsOrder
	if err := json.Unmarshal(env.Data, &order); err != nil {
		return fmt.Errorf("%w: lemonsqueezy refund: %v", ErrMalformedEvent, err)
	}
	refunded := order.Attributes.RefundedAmount // cumulative, minor units
	if refunded <= 0 {
		return nil
	}
	refundKey := "order_refunded:" + order.ID + ":" + strconv.FormatInt(refunded, 10)
	// Refund attribution: the purchase linked the commission to the order
	// id and the order identifier (keys); refunds carry the same ids.
	return a.clawCumulative(ctx, refundKey, strings.ToUpper(order.Attributes.Currency),
		eventTime(order.Attributes.RefundedAt, order.Attributes.UpdatedAt),
		refunded, order.Attributes.Total, order.ID, order.Attributes.Identifier)
}

// clawCumulative claws the commission linked to keys down to the
// cumulative refunded total. total is the sale total (tax included, like
// refunded_amount) used for proration.
func (a *LemonSqueezyAdapter) clawCumulative(ctx context.Context, refundKey, currency string, occurred time.Time, refunded, total int64, keys ...string) error {
	ev := commissions.Event{
		Source:     "lemonsqueezy",
		ExternalID: refundKey,
		EventType:  "refund",
		Currency:   currency,
		OccurredAt: occurred,
	}
	commissionID, found, err := a.Store.FindCommission(ctx, "lemonsqueezy", keys...)
	if err != nil {
		return err
	}
	if !found {
		if err := a.Store.RecordRefundMiss(ctx, "lemonsqueezy", refundKey, keys, refunded, currency); err != nil {
			return err
		}
		// The purchase may still come: it applies the held refund.
		return holdMissedRefund(ctx, a.Engine, a.Store,
			heldRefund{Event: ev, Refunded: refunded, Original: total, Cumulative: true}, keys...)
	}
	// Proration base: the sale total (the sale gross); when absent, the
	// commission's stored gross (refundBase), else the refunded amount.
	original := total
	if original <= 0 {
		original = refundBase(ctx, a.Store, commissionID, refunded)
	}
	if cr, ok := a.Engine.(CumulativeRefunder); ok {
		err = cr.ProcessRefundCumulative(ctx, ev, commissionID, refunded, original)
	} else {
		err = a.Engine.ProcessRefund(ctx, ev, commissionID, refunded, original)
	}
	if errors.Is(err, commissions.ErrAlreadyProcessed) {
		return nil // replayed refund delivery: success no-op
	}
	return err
}

// resolveAndPurchase runs the attribution ladder, credits, and links the
// commission to keys (the refund path finds it by them). basis is the
// tax-inclusive total refunds prorate against.
func (a *LemonSqueezyAdapter) resolveAndPurchase(ctx context.Context, ev *commissions.Event, basis int64, keys ...string) error {
	// Full ladder query (Paddle/Stripe pattern): coupon + token + customer
	// rungs. The token rung lives HERE, not in the engine — the engine's
	// internal ladder re-resolves coupon/customer only.
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		CouponCode:    ev.CouponCode,
		CustomerExtID: ev.CustomerExtID,
		Token:         ev.ClickToken,
		CampaignID:    ev.CampaignID,
		Now:           ev.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("lemonsqueezy: attribution: %w", err)
	}
	if res.Flagged {
		return nil
	}
	if res.Method != "" {
		ev.AffiliateID = res.AffiliateID
		ev.Method = res.Method
		if res.CampaignID != 0 {
			ev.CampaignID = res.CampaignID
		}
		if res.ClickID != "" {
			ev.ClickID = res.ClickID
		}
	}
	cid, err := a.Engine.ProcessPurchase(ctx, *ev)
	switch {
	case errors.Is(err, commissions.ErrAlreadyProcessed):
		// Replay: success no-op, after finishing a link an earlier
		// delivery may have failed to write.
		return relinkReplay(ctx, a.Engine, a.Store, *ev, basis, keys...)
	case errors.Is(err, commissions.ErrNoAttribution):
		// Organic sale: permanent condition — ACK so LS stops retrying
		// (the event is recorded in raw_events as "unattributed").
		log.Printf("billing: lemonsqueezy/%s unattributed (recorded, skipped)", ev.ExternalID)
		return nil
	case err != nil:
		return err
	}
	return linkCommission(ctx, a.Engine, a.Store, *ev, cid, basis, keys...)
}
