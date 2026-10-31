// polar.go — Polar webhook adapter.
//
// Signature (polar.sh/docs/integrate/webhooks/delivery): Standard
// Webhooks. Headers webhook-id, webhook-timestamp and webhook-signature
// ("v1,<base64>" entries, space-separated); the signed content is
// "<id>.<timestamp>.<raw body>", HMAC-SHA256. The key is the whsec_
// secret base64-decoded (Standard Webhooks, Polar's scheme from
// 2026-09-08); Polar's earlier scheme keyed the HMAC with the UTF-8 bytes
// of the whole whsec_ string, which is accepted too so deliveries signed
// either way verify. Timestamps more than 5 minutes off are refused.
//
// Payload: {type, timestamp, data: Order}. Money fields are integer minor
// units. The commissionable amount is net_amount (after discounts, before
// tax), like Paddle's.
//
// Attribution: t.js adds the token to Polar checkout links as
// reference_id, which Polar copies into the checkout's metadata and on to
// the order (and the subscription's later orders). metadata.aff_token is
// read too, for merchants who create checkouts over the API.
//
// Every paid order credits: the first purchase, and each subscription
// renewal or change (Polar makes an order for every charge). A $0 order
// (a free trial) binds the customer to the partner without crediting, so
// the first paid renewal earns through the binding. order.refunded carries
// the cumulative refunded_amount and claws back that share.
//
// Idempotency keys: "order.paid:<order id>" and
// "order.refunded:<order id>:<cumulative refunded>".
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

// polarTolerance bounds how far a webhook timestamp may be from now.
const polarTolerance = 5 * time.Minute

// PolarAdapter verifies and maps Polar order webhooks.
type PolarAdapter struct {
	Secret   string // the endpoint's whsec_ secret
	Engine   ProcessPurchaser
	Resolver AttributionResolver
	Store    Store
	Now      func() time.Time // tests
}

// NewPolarAdapter builds one.
func NewPolarAdapter(secret string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *PolarAdapter {
	return &PolarAdapter{Secret: strings.TrimSpace(secret), Engine: engine, Resolver: resolver, Store: store}
}

type polarEnvelope struct {
	Type string `json:"type"`
	// Timestamp is when the event happened (RFC 3339); retries keep it.
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type polarOrder struct {
	ID             string                     `json:"id"`
	Status         string                     `json:"status"`
	Paid           bool                       `json:"paid"`
	NetAmount      int64                      `json:"net_amount"`
	TotalAmount    int64                      `json:"total_amount"`
	RefundedAmount int64                      `json:"refunded_amount"`
	Currency       string                     `json:"currency"`
	BillingReason  string                     `json:"billing_reason"`
	CustomerID     string                     `json:"customer_id"`
	ProductID      string                     `json:"product_id"`
	SubscriptionID string                     `json:"subscription_id"`
	InvoiceNumber  string                     `json:"invoice_number"`
	CreatedAt      string                     `json:"created_at"`
	Metadata       map[string]json.RawMessage `json:"metadata"`
	Customer       struct {
		Email string `json:"email"`
	} `json:"customer"`
}

// polarKeys are the HMAC keys a delivery may be signed with.
func (a *PolarAdapter) polarKeys() [][]byte {
	keys := [][]byte{}
	if rest, ok := strings.CutPrefix(a.Secret, "whsec_"); ok {
		if k, err := base64.StdEncoding.DecodeString(rest); err == nil && len(k) > 0 {
			keys = append(keys, k)
		}
	}
	return append(keys, []byte(a.Secret))
}

func (a *PolarAdapter) verify(raw []byte, h http.Header) error {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return fmt.Errorf("%w: missing webhook-id, webhook-timestamp or webhook-signature", ErrInvalidSignature)
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad webhook-timestamp", ErrInvalidSignature)
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if d := now.Sub(time.Unix(sec, 0)); d > polarTolerance || d < -polarTolerance {
		return fmt.Errorf("%w: timestamp outside 5 minutes", ErrInvalidSignature)
	}
	content := append([]byte(id+"."+ts+"."), raw...)
	for _, key := range a.polarKeys() {
		m := hmac.New(sha256.New, key)
		m.Write(content)
		want := m.Sum(nil)
		for _, s := range strings.Fields(sigs) {
			v, b64, ok := strings.Cut(s, ",")
			if !ok || v != "v1" {
				continue
			}
			got, err := base64.StdEncoding.DecodeString(b64)
			if err == nil && hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
}

// HandleWebhook implements Adapter.
func (a *PolarAdapter) HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error {
	if err := a.verify(rawBody, headers); err != nil {
		return err
	}
	var env polarEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return fmt.Errorf("%w: polar envelope: %v", ErrMalformedEvent, err)
	}
	switch env.Type {
	case "order.paid":
		return a.handlePaid(ctx, env)
	case "order.refunded":
		return a.handleRefunded(ctx, env)
	}
	return nil // other events: forward-compatible success
}

func polarMeta(md map[string]json.RawMessage, key string) string {
	raw, ok := md[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func polarToken(o polarOrder) string {
	if t := polarMeta(o.Metadata, "reference_id"); t != "" {
		return t
	}
	return polarMeta(o.Metadata, "aff_token")
}

func (a *PolarAdapter) handlePaid(ctx context.Context, env polarEnvelope) error {
	var o polarOrder
	if err := json.Unmarshal(env.Data, &o); err != nil || o.ID == "" {
		return fmt.Errorf("%w: polar order", ErrMalformedEvent)
	}
	if !o.Paid && o.Status != "paid" {
		return nil
	}
	if o.NetAmount <= 0 {
		return a.bind(ctx, o) // free trial or 100% discount: bind for renewals
	}
	ev := commissions.Event{
		Source:        "polar",
		ExternalID:    "order.paid:" + o.ID,
		EventType:     "purchase",
		AmountMinor:   o.NetAmount,
		Currency:      strings.ToUpper(o.Currency),
		CustomerExtID: o.CustomerID,
		CustomerEmail: o.Customer.Email,
		ClickToken:    polarToken(o),
		CouponCode:    polarMeta(o.Metadata, "coupon"),
		ProductKey:    o.ProductID,
		CampaignID:    1,
		OccurredAt:    eventTime(env.Timestamp, o.CreatedAt),
		OrderID:       firstNonEmpty(o.InvoiceNumber, o.ID),
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		CouponCode: ev.CouponCode, CustomerExtID: ev.CustomerExtID, Token: ev.ClickToken,
		CampaignID: ev.CampaignID, Now: ev.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("polar: attribution: %w", err)
	}
	if res.Flagged {
		return nil
	}
	if res.Method != "" {
		ev.AffiliateID, ev.Method = res.AffiliateID, res.Method
		if res.CampaignID != 0 {
			ev.CampaignID = res.CampaignID
		}
		if res.ClickID != "" {
			ev.ClickID = res.ClickID
		}
	}
	cid, err := a.Engine.ProcessPurchase(ctx, ev)
	switch {
	case errors.Is(err, commissions.ErrAlreadyProcessed):
		// Replay: finish a link an earlier delivery may have failed to write.
		return relinkReplay(ctx, a.Engine, a.Store, ev, 0, o.ID)
	case errors.Is(err, commissions.ErrNoAttribution):
		log.Printf("billing: polar/%s unattributed (recorded, skipped)", ev.ExternalID)
		return nil
	case err != nil:
		return err
	}
	// No refund basis: Polar's refunded_amount excludes tax, like net_amount.
	return linkCommission(ctx, a.Engine, a.Store, ev, cid, 0, o.ID)
}

// bind writes the customer's lifetime binding from the checkout token.
func (a *PolarAdapter) bind(ctx context.Context, o polarOrder) error {
	tok := polarToken(o)
	binder, ok := a.Engine.(CustomerBinder)
	if tok == "" || o.CustomerID == "" || a.Resolver == nil || !ok {
		return nil
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{Token: tok, CampaignID: 1, Now: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("polar: resolve checkout token: %w", err)
	}
	if res.Method != "token" {
		return nil
	}
	return binder.BindCustomer(ctx, o.CustomerID, res.AffiliateID, res.CampaignID, "token")
}

func (a *PolarAdapter) handleRefunded(ctx context.Context, env polarEnvelope) error {
	var o polarOrder
	if err := json.Unmarshal(env.Data, &o); err != nil || o.ID == "" {
		return fmt.Errorf("%w: polar refund", ErrMalformedEvent)
	}
	refunded := o.RefundedAmount
	if refunded <= 0 {
		return nil
	}
	// The commission was on net_amount; a refund can't claw more than it.
	base := o.NetAmount
	if base > 0 && refunded > base {
		refunded = base
	}
	cur := strings.ToUpper(o.Currency)
	refundKey := "order.refunded:" + o.ID + ":" + strconv.FormatInt(refunded, 10)
	ev := commissions.Event{Source: "polar", ExternalID: refundKey, EventType: "refund", Currency: cur, OccurredAt: eventTime(env.Timestamp)}
	commissionID, found, err := a.Store.FindCommission(ctx, "polar", o.ID)
	if err != nil {
		return err
	}
	if !found {
		if err := a.Store.RecordRefundMiss(ctx, "polar", refundKey, []string{o.ID}, refunded, cur); err != nil {
			return err
		}
		// The purchase may still come: it applies the held refund.
		return holdMissedRefund(ctx, a.Engine, a.Store,
			heldRefund{Event: ev, Refunded: refunded, Original: base, Cumulative: true}, o.ID)
	}
	if base <= 0 {
		base = refundBase(ctx, a.Store, commissionID, refunded)
	}
	if cr, ok := a.Engine.(CumulativeRefunder); ok {
		err = cr.ProcessRefundCumulative(ctx, ev, commissionID, refunded, base)
	} else {
		err = a.Engine.ProcessRefund(ctx, ev, commissionID, refunded, base)
	}
	if errors.Is(err, commissions.ErrAlreadyProcessed) {
		return nil
	}
	return err
}
