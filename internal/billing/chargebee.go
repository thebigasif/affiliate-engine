// Chargebee webhook adapter — third billing adapter (the interface is
// proven; this one follows the established contract).
//
// Authentication. Chargebee webhooks authenticate with HTTP Basic Auth:
// the username/password are configured on the webhook in the Chargebee
// console and sent as a standard "Authorization: Basic ..." header. Set
// Username and/or Password (NewChargebeeBasicAuthAdapter) to require it;
// the whole header is compared in constant time (Recurly pattern).
//
// Legacy HMAC mode: when no Basic Auth credentials are configured, the
// adapter falls back to the original scheme — header "cb-signature" =
// base64(HMAC-SHA256(secret, rawBody)). When Basic Auth IS configured and a
// secret is too, a cb-signature header (if present) must also verify.
// Either way authentication precedes any parse or DB write; replays are
// deduped by the engine.
//
// Attribution. Chargebee checkouts carry no custom data, so the merchant
// adds a Customers custom field with API name cf_aff_token and their
// server sets it (customer[cf_aff_token]) to the visitor's at_token when
// it opens checkout. payment_succeeded reads content.customer.cf_aff_token
// for the token rung; renewals credit through the customer binding. A $0
// invoice (trial) and subscription_created / customer_created bind the
// customer without crediting, so the first paid invoice earns.
//
// Refunds. One refund usually sends both credit_note_created and
// payment_refunded. Both are keyed by the credit note id, so whichever
// arrives second is a replay. Adjustment credit notes (no money back) are
// ignored.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// ChargebeeAdapter verifies and maps Chargebee webhooks.
type ChargebeeAdapter struct {
	Secret   string // legacy cb-signature HMAC secret ("" = HMAC disabled)
	Username string // Basic Auth username (Chargebee webhook setting)
	Password string // Basic Auth password (Chargebee webhook setting)
	Engine   ProcessPurchaser
	Resolver AttributionResolver
	Store    Store
}

// NewChargebeeAdapter builds one in legacy HMAC (cb-signature) mode.
func NewChargebeeAdapter(secret string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *ChargebeeAdapter {
	return &ChargebeeAdapter{Secret: secret, Engine: engine, Resolver: resolver, Store: store}
}

// NewChargebeeBasicAuthAdapter builds one authenticating with HTTP Basic
// Auth, Chargebee's native webhook protection. At least one of username /
// password must be non-empty.
func NewChargebeeBasicAuthAdapter(username, password string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *ChargebeeAdapter {
	return &ChargebeeAdapter{
		Username: username, Password: password,
		Engine: engine, Resolver: resolver, Store: store,
	}
}

// cbEvent is the flattened Chargebee envelope: {id, event_type, content:{...}}.
type cbEvent struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Content   json.RawMessage `json:"content"`
	// OccurredAt is when the event happened (Unix seconds); retries keep it.
	OccurredAt int64 `json:"occurred_at"`
}

type cbInvoice struct {
	// Chargebee's invoice object carries its id as "id"; invoice_id is the
	// flattened alias older fixtures use. Either identifies the invoice.
	ID             string `json:"id"`
	InvoiceID      string `json:"invoice_id"`
	CustomerID     string `json:"customer_id"`
	BillingAddress struct {
		Email   string `json:"email"`
		Country string `json:"country"`
	} `json:"billing_address"`
	CurrencyCode string `json:"currency_code"`
	Total        int64  `json:"total"`  // minor units, after discounts, tax included
	Tax          int64  `json:"tax"`    // minor units: the tax part of total
	Status       string `json:"status"` // "paid" | "posted" | "payment_due" | "not_paid" | "voided" | "pending"
	// Line items carry the plan identity for per-product rates (FR-12):
	// entity_id is the item price / plan price point id.
	LineItems []cbLineItem `json:"line_items"`
}

// cbCustomer is content.customer (sent alongside the invoice). Custom
// fields arrive as top-level cf_* keys.
type cbCustomer struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	AffToken string `json:"cf_aff_token"`
}

// cbToken returns the attribution token from the customer's cf_aff_token:
// the raw at_token value, or its at1_ client-reference form.
func cbToken(c cbCustomer) string {
	t := strings.TrimSpace(c.AffToken)
	if v, ok := tokens.FromClientReferenceID(t); ok {
		return v
	}
	return t
}

type cbLineItem struct {
	ID       string `json:"entity_id"`
	Quantity int    `json:"quantity"`
}

// HandleWebhook implements Adapter.
func (a *ChargebeeAdapter) HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error {
	// 1) Authenticate FIRST.
	if err := a.verifyAuth(rawBody, headers); err != nil {
		return err
	}

	// 2) Envelope (auth covers the exact parsed bytes).
	var env cbEvent
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return fmt.Errorf("%w: chargebee envelope: %v", ErrMalformedEvent, err)
	}

	// Chargebee event types are snake_case (payment_succeeded,
	// credit_note_created); the dotted names are older fixtures.
	switch env.EventType {
	case "payment_succeeded", "invoice.paid":
		return a.handleInvoice(ctx, env)
	case "credit_note_created", "payment_refunded", "invoice.credit_note_created":
		return a.handleRefund(ctx, env)
	case "subscription_created", "customer_created":
		return a.handleBind(ctx, env)
	default:
		return nil // forward-compatible silent success
	}
}

// basicAuthConfigured reports whether Basic Auth credentials are set.
func (a *ChargebeeAdapter) basicAuthConfigured() bool {
	return a.Username != "" || a.Password != ""
}

// verifyAuth: Basic Auth when configured (plus cb-signature when both a
// secret and the header are present); otherwise the legacy HMAC path.
func (a *ChargebeeAdapter) verifyAuth(rawBody []byte, headers http.Header) error {
	sig := headers.Get("cb-signature")
	if !a.basicAuthConfigured() {
		if a.Secret == "" {
			return fmt.Errorf("%w: chargebee auth not configured", ErrInvalidSignature)
		}
		return a.verifySignature(rawBody, sig)
	}
	expected := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(a.Username+":"+a.Password))
	got := headers.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return fmt.Errorf("%w: chargebee basic auth", ErrInvalidSignature)
	}
	if sig != "" && a.Secret != "" {
		return a.verifySignature(rawBody, sig)
	}
	return nil
}

func (a *ChargebeeAdapter) handleInvoice(ctx context.Context, env cbEvent) error {
	// Chargebee nests entities under content.{invoice,customer,...}.
	var wrapped struct {
		Invoice  cbInvoice  `json:"invoice"`
		Customer cbCustomer `json:"customer"`
	}
	if err := json.Unmarshal(env.Content, &wrapped); err != nil {
		return fmt.Errorf("%w: chargebee invoice: %v", ErrMalformedEvent, err)
	}
	inv := wrapped.Invoice
	if inv.Total <= 0 {
		// $0 invoice (trial, 100% discount): nothing to credit, but a
		// tokened customer is bound so the first paid invoice earns.
		return a.bind(ctx, firstNonEmpty(inv.CustomerID, wrapped.Customer.ID), wrapped.Customer)
	}
	// Only a paid invoice is money that landed (status absent = legacy
	// flattened payloads, accepted as before).
	if inv.Status != "" && inv.Status != "paid" {
		return nil
	}
	// Commission on the tax-exclusive amount (Stripe/Paddle/Polar parity).
	amount := exTax(inv.Total, inv.Tax)
	if amount <= 0 {
		return nil
	}
	invoiceID := firstNonEmpty(inv.InvoiceID, inv.ID)
	ev := commissions.Event{
		Source:        "chargebee",
		ExternalID:    env.ID,
		EventType:     "purchase",
		AmountMinor:   amount,
		Currency:      strings.ToUpper(inv.CurrencyCode),
		CustomerExtID: firstNonEmpty(inv.CustomerID, wrapped.Customer.ID),
		// Buyer email: the customer record, else the invoice billing
		// address (both nested objects in Chargebee payloads).
		CustomerEmail: firstNonEmpty(wrapped.Customer.Email, inv.BillingAddress.Email),
		Country:       strings.ToUpper(inv.BillingAddress.Country),
		// Per-product rates (FR-12): first line item's entity_id (the
		// item-price / plan price point).
		ProductKey: firstCbLineItem(inv.LineItems),
		ClickToken: cbToken(wrapped.Customer),
		CampaignID: 1,
		OccurredAt: unixTime(env.OccurredAt),
		// Postback {order_id}: the Chargebee invoice id.
		OrderID: invoiceID,
	}
	// Credit notes and refunds include tax: they prorate against the
	// invoice total (the refund basis), not the tax-exclusive gross.
	return a.resolveAndPurchase(ctx, &ev, inv.Total, invoiceID)
}

// handleBind binds a tokened customer from subscription_created or
// customer_created (no money moves).
func (a *ChargebeeAdapter) handleBind(ctx context.Context, env cbEvent) error {
	var wrapped struct {
		Customer cbCustomer `json:"customer"`
	}
	if err := json.Unmarshal(env.Content, &wrapped); err != nil {
		return fmt.Errorf("%w: chargebee customer: %v", ErrMalformedEvent, err)
	}
	return a.bind(ctx, wrapped.Customer.ID, wrapped.Customer)
}

// bind writes the lifetime customer binding from cf_aff_token without a
// credit (the Stripe subscription-checkout pattern). No token, customer,
// resolver or binder capability = nothing to do.
func (a *ChargebeeAdapter) bind(ctx context.Context, customerID string, c cbCustomer) error {
	tok := cbToken(c)
	binder, ok := a.Engine.(CustomerBinder)
	if tok == "" || customerID == "" || a.Resolver == nil || !ok {
		return nil
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{Token: tok, CampaignID: 1, Now: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("chargebee: resolve cf_aff_token: %w", err)
	}
	if res.Method != "token" {
		log.Printf("billing: chargebee: cf_aff_token invalid, expired or affiliate not active (customer %s not bound)", customerID)
		return nil
	}
	return binder.BindCustomer(ctx, customerID, res.AffiliateID, res.CampaignID, "token")
}

// firstCbLineItem returns the first line item's entity_id (the purchased
// plan's price-point identity); "" when the invoice carries no lines.
func firstCbLineItem(items []cbLineItem) string {
	if len(items) == 0 {
		return ""
	}
	return items[0].ID
}

func (a *ChargebeeAdapter) handleRefund(ctx context.Context, env cbEvent) error {
	var wrapped struct {
		CreditNote struct {
			ID                 string `json:"id"`
			CreditNoteID       string `json:"cn_id"` // older fixtures
			InvoiceID          string `json:"invoice_id"`
			ReferenceInvoiceID string `json:"reference_invoice_id"` // Chargebee credit_note field
			Type               string `json:"type"`                 // "refundable" | "adjustment"
			CurrencyCode       string `json:"currency_code"`
			Total              int64  `json:"total"` // minor units (older fixtures: negative)
		} `json:"credit_note"`
		Transaction struct {
			ID             string `json:"id"`
			Amount         int64  `json:"amount"`
			CurrencyCode   string `json:"currency_code"`
			LinkedInvoices []struct {
				InvoiceID string `json:"invoice_id"`
			} `json:"linked_invoices"`
		} `json:"transaction"`
		Invoice cbInvoice `json:"invoice"`
	}
	if err := json.Unmarshal(env.Content, &wrapped); err != nil {
		return fmt.Errorf("%w: chargebee refund: %v", ErrMalformedEvent, err)
	}
	cn, txn := wrapped.CreditNote, wrapped.Transaction
	var key, invoiceID, currency string
	var refunded int64
	switch cnID := firstNonEmpty(cn.ID, cn.CreditNoteID); {
	case cnID != "":
		if cn.Type == "adjustment" {
			return nil // a price adjustment, not money back
		}
		key = "credit_note:" + cnID
		invoiceID = firstNonEmpty(cn.ReferenceInvoiceID, cn.InvoiceID)
		refunded, currency = cn.Total, cn.CurrencyCode
	case txn.ID != "" && env.EventType == "payment_refunded":
		// A refund with no credit note in the payload: key it by the
		// refund transaction.
		key = "refund_txn:" + txn.ID
		if len(txn.LinkedInvoices) > 0 {
			invoiceID = txn.LinkedInvoices[0].InvoiceID
		}
		invoiceID = firstNonEmpty(invoiceID, firstNonEmpty(wrapped.Invoice.ID, wrapped.Invoice.InvoiceID))
		refunded, currency = txn.Amount, txn.CurrencyCode
	default:
		return nil
	}
	if refunded < 0 {
		refunded = -refunded
	}
	if refunded == 0 || invoiceID == "" {
		return nil
	}
	currency = strings.ToUpper(currency)
	if currency == "" {
		currency = "USD"
	}
	// Find the parent via the invoice-id link (purchases link event+invoice).
	commissionID, found, err := a.Store.FindCommission(ctx, "chargebee", invoiceID)
	if err != nil {
		return err
	}
	if !found {
		return a.Store.RecordRefundMiss(ctx, "chargebee", key,
			[]string{invoiceID}, refunded, currency)
	}
	// Proration base = the invoice total recorded at credit time (credit
	// notes include tax, like it); older commissions, credited on that
	// total, use their gross; legacy rows fall back to the commission
	// amount, then the refunded amount (full clawback).
	original := refundBase(ctx, a.Store, commissionID, refunded)
	ev := commissions.Event{
		Source: "chargebee", ExternalID: key, EventType: "refund",
		Currency: currency, OccurredAt: unixTime(env.OccurredAt),
	}
	err = a.Engine.ProcessRefund(ctx, ev, commissionID, refunded, original)
	if errors.Is(err, commissions.ErrAlreadyProcessed) {
		return nil // the same credit note via the other event, or a replay
	}
	return err
}

func (a *ChargebeeAdapter) resolveAndPurchase(ctx context.Context, ev *commissions.Event, basis int64, invoiceID string) error {
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		CustomerExtID: ev.CustomerExtID,
		Token:         ev.ClickToken,
		CampaignID:    ev.CampaignID,
		Now:           ev.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("chargebee: attribution: %w", err)
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
		// Click linkage for postback {click_id} (Stripe pattern).
		if res.ClickID != "" {
			ev.ClickID = res.ClickID
		}
	}
	cid, err := a.Engine.ProcessPurchase(ctx, *ev)
	switch {
	case errors.Is(err, commissions.ErrAlreadyProcessed):
		// Replay: success no-op, after finishing a link an earlier
		// delivery may have failed to write.
		return relinkReplay(ctx, a.Engine, a.Store, *ev, basis, ev.ExternalID, invoiceID)
	case errors.Is(err, commissions.ErrNoAttribution):
		// Organic sale: permanent condition — ACK so Chargebee stops
		// retrying (the event is recorded in raw_events as "unattributed").
		log.Printf("billing: chargebee/%s unattributed (recorded, skipped)", ev.ExternalID)
		return nil
	case err != nil:
		return err
	}
	return linkCommission(ctx, a.Store, *ev, cid, basis, ev.ExternalID, invoiceID)
}

// verifySignature checks the legacy cb-signature header:
// base64(HMAC-SHA256(secret, rawBody)), compared timing-safe on the
// decoded bytes.
func (a *ChargebeeAdapter) verifySignature(rawBody []byte, header string) error {
	if header == "" {
		return fmt.Errorf("%w: missing cb-signature", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write(rawBody)
	got, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return fmt.Errorf("%w: bad base64 signature", ErrInvalidSignature)
	}
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
	}
	return nil
}
