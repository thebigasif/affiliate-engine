// recurly.go — Recurly webhook adapter (FR-31, 5th of 5 PRD billing
// providers — completes the FR-31 set).
//
// Recurly webhook endpoints are configured per-endpoint for EITHER JSON or
// XML. JSON payloads are metadata-only (event ids, no amounts — Recurly's
// stated model is "webhooks as triggers, fetch the rest via API"), so this
// adapter consumes the XML payload type, which carries the full invoice /
// transaction / account data needed to commission without an API round
// trip and an API key.
//
// Auth is layered, verify-before-parse-or-write like every adapter:
//
//  1. HTTP Basic Auth (credentials configured on the endpoint in the
//     Recurly console) — constant-time whole-header compare. Recurly
//     permits either the username or the password to be blank; the route
//     is enabled when at least one is set.
//  2. recurly-signature header, when present: "<ts-millis>,<hex sig>[,...]"
//     where sig = HMAC-SHA256(secret, ts + "." + rawBody); any one match
//     verifies (secret rotation keeps old+new valid for 24h). Algorithm
//     and the 5-minute tolerance mirror Recurly's official Go client
//     (recurly-client-go webhook.go). XML payloads are NOT signed per the
//     docs, so the header is normally absent — but if one arrives and no
//     secret is configured, the delivery is REJECTED (fail-closed on
//     anomalous auth material: it either came from a JSON-mode endpoint
//     we did not configure, or an attacker).
//
// Mapped notifications (XML root element = event type):
//
//	closed_invoice_notification       (legacy)   -> purchase (state=collected)
//	paid_charge_invoice_notification  (Credit
//	 Invoices era)                    -> purchase (state=paid)
//	successful_refund_notification    -> refund (clawback)
//	void_payment_notification         -> refund (void after capture = clawback)
//	everything else                   -> silent success (forward-compatible)
//
// Idempotency keys (XML notifications carry no notification id — the JSON
// envelope's "id" does not exist in XML):
//
//	purchase -> "invoice:<uuid>:<closed_at|date>"  — the timestamp makes a
//	           reopened-then-reclosed manual invoice a DISTINCT event, so
//	           the second close re-credits what the refund clawed back;
//	           replaying the same delivery still dedupes.
//	refund   -> "refund:<transaction id>"          — Recurly mints a new
//	           transaction per refund action, so successive partial
//	           refunds are distinct while replays dedupe.
//
// Attribution: notification XML carries no custom fields. The merchant
// adds an Accounts custom field aff_token and sets it (the visitor's
// at_token) when their server creates the account. With a read-only API
// key (AccountToken, AE_RECURLY_API_KEY) the adapter reads that field for
// an account that is not bound yet: on its first paid invoice (token
// rung, then bound), and on new_account / new_subscription notifications
// and $0 invoices (bound without a credit, so trials keep their
// affiliate). Bound accounts credit through the customer binding and cost
// no API call. Without a key, only the binding rung applies. Per-product rate keys
// are likewise not derivable from notification XML (line items live in
// the API), so ProductKey is left empty and campaign-default rates apply.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// RecurlySignatureTolerance mirrors recurly-client-go's
// DEFAULT_WEBHOOK_TOLERANCE (5 minutes), applied symmetrically
// (future-dated timestamps are as suspect as stale ones).
const RecurlySignatureTolerance = 5 * time.Minute

// RecurlyAdapter authenticates and maps Recurly XML webhook notifications.
type RecurlyAdapter struct {
	Username string // Basic Auth username ("" allowed — Recurly permits blank)
	Password string // Basic Auth password ("" allowed)
	Secret   string // recurly-signature secret ("" = XML mode, no signature expected)
	Engine   ProcessPurchaser
	Resolver AttributionResolver
	Store    Store

	// AccountToken reads an account's aff_token custom field through the
	// Recurly API (RecurlyAccountToken); nil = no API key configured.
	AccountToken func(ctx context.Context, accountCode string) (string, error)
}

// NewRecurlyAdapter builds one. username/password gate the route (at least
// one non-empty); secret is optional (signature verification when the
// header appears).
func NewRecurlyAdapter(username, password, secret string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *RecurlyAdapter {
	return &RecurlyAdapter{
		Username: username, Password: password, Secret: secret,
		Engine: engine, Resolver: resolver, Store: store,
	}
}

// rxNotification is any Recurly XML notification; the root element name is
// the event type. Only the blocks we commission on are decoded.
type rxNotification struct {
	XMLName xml.Name
	Account struct {
		AccountCode string `xml:"account_code"`
		Email       string `xml:"email"`
	} `xml:"account"`
	Invoice     *rxInvoice     `xml:"invoice"`
	Transaction *rxTransaction `xml:"transaction"`
}

type rxInvoice struct {
	UUID                string   `xml:"uuid"`
	State               string   `xml:"state"`  // collected|paid = money landed
	Origin              string   `xml:"origin"` // purchase|renewal|immediate|...
	InvoiceNumberPrefix string   `xml:"invoice_number_prefix"`
	InvoiceNumber       int64    `xml:"invoice_number"`
	TotalInCents        int64    `xml:"total_in_cents"` // after discount, tax included
	TaxInCents          int64    `xml:"tax_in_cents"`
	Currency            string   `xml:"currency"`
	Date                string   `xml:"date"`
	ClosedAt            string   `xml:"closed_at"`
	SubscriptionIDs     []string `xml:"subscription_ids>subscription_id"`
}

type rxTransaction struct {
	ID                  string `xml:"id"`
	InvoiceID           string `xml:"invoice_id"`
	InvoiceNumberPrefix string `xml:"invoice_number_prefix"`
	InvoiceNumber       int64  `xml:"invoice_number"`
	Action              string `xml:"action"` // "credit" on refunds
	AmountInCents       int64  `xml:"amount_in_cents"`
	Date                string `xml:"date"`
}

// HandleWebhook implements Adapter.
func (a *RecurlyAdapter) HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error {
	// 1) Auth FIRST — before any parse or DB write.
	if err := a.verifyAuth(rawBody, headers); err != nil {
		return err
	}

	// 2) Decode; the root element name dispatches.
	var n rxNotification
	if err := xml.Unmarshal(rawBody, &n); err != nil {
		return fmt.Errorf("%w: recurly xml: %v", ErrMalformedEvent, err)
	}

	switch name := n.XMLName.Local; name {
	case "closed_invoice_notification", "paid_charge_invoice_notification":
		return a.handleInvoicePaid(ctx, n)
	case "successful_refund_notification":
		return a.handleRefund(ctx, n, "refund")
	case "void_payment_notification":
		return a.handleRefund(ctx, n, "void")
	case "new_account_notification", "new_subscription_notification":
		return a.bind(ctx, n.Account.AccountCode)
	default:
		return nil // account/subscription/dunning/item/...: silent success
	}
}

// verifyAuth checks Basic Auth (always) and the recurly-signature header
// (when present) over the exact bytes that will be parsed.
func (a *RecurlyAdapter) verifyAuth(rawBody []byte, headers http.Header) error {
	expected := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(a.Username+":"+a.Password))
	got := headers.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return fmt.Errorf("%w: recurly basic auth", ErrInvalidSignature)
	}
	if sig := headers.Get("recurly-signature"); sig != "" {
		if a.Secret == "" {
			// Signed delivery against an unsigned endpoint config: not a
			// shape Recurly produces for XML endpoints — refuse it.
			return fmt.Errorf("%w: recurly-signature present but no secret configured", ErrInvalidSignature)
		}
		if err := verifyRecurlySignature(sig, a.Secret, rawBody, RecurlySignatureTolerance, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// verifyRecurlySignature implements the recurly-client-go algorithm:
// header "<ts>,<sig>[,...]"; ts is Unix MILLIseconds; message is
// ts + "." + body; HMAC-SHA256; any signature matching verifies.
func verifyRecurlySignature(header, secret string, body []byte, tolerance time.Duration, now time.Time) error {
	parts := strings.Split(header, ",")
	if len(parts) < 2 {
		return fmt.Errorf("%w: malformed recurly-signature", ErrInvalidSignature)
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad recurly-signature ts", ErrInvalidSignature)
	}
	sent := time.Unix(ts/1e3, (ts%1e3)*1e6)
	if d := now.Sub(sent); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: stale recurly-signature ts", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0]))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range parts[1:] {
		got, err := hex.DecodeString(s)
		if err != nil {
			continue // malformed candidate: not the one that will match
		}
		if hmac.Equal(want, got) {
			return nil
		}
	}
	return fmt.Errorf("%w: recurly-signature mismatch", ErrInvalidSignature)
}

// handleInvoicePaid maps a collected/paid invoice to a purchase.
func (a *RecurlyAdapter) handleInvoicePaid(ctx context.Context, n rxNotification) error {
	inv := n.Invoice
	if inv == nil || inv.UUID == "" {
		return fmt.Errorf("%w: recurly invoice missing", ErrMalformedEvent)
	}
	// Legacy closed fires for failed collection too; only money that
	// landed is commissionable.
	switch inv.State {
	case "collected", "paid":
	default:
		return nil
	}
	if inv.TotalInCents <= 0 {
		// $0 (trial, 100% discount): nothing to credit, but bind a
		// tokened account so its first paid invoice earns.
		return a.bind(ctx, n.Account.AccountCode)
	}
	// Commission on the tax-exclusive amount (Stripe/Paddle/Polar parity).
	amount := exTax(inv.TotalInCents, inv.TaxInCents)
	if amount <= 0 {
		return nil
	}

	// occurred: the moment the invoice closed (paid); creation date as
	// the fallback. Also folds into the idempotency key so a
	// reopen->reclose cycle re-credits (see package comment).
	occurred, ok := rxTime(inv.ClosedAt)
	if !ok {
		occurred, _ = rxTime(inv.Date)
	}
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}

	ev := commissions.Event{
		Source:        "recurly",
		ExternalID:    "invoice:" + inv.UUID + ":" + strconv.FormatInt(occurred.Unix(), 10),
		EventType:     rxEventType(inv.Origin),
		AmountMinor:   amount, // Recurly sends minor units already
		Currency:      strings.ToUpper(inv.Currency),
		CustomerExtID: n.Account.AccountCode,
		CustomerEmail: n.Account.Email,
		// No token/coupon rung: Recurly notification XML carries no
		// order-time custom data (attribution runs on the customer
		// binding rung). See package comment.
		CampaignID: 1,
		OccurredAt: occurred,
		// Postback {order_id}: the merchant-facing invoice reference.
		OrderID: inv.InvoiceNumberPrefix + strconv.FormatInt(inv.InvoiceNumber, 10),
	}
	return a.resolveAndPurchase(ctx, &ev, inv)
}

// rxEventType maps invoice origin to the engine's event vocabulary:
// renewals keep their recurring semantics; everything else (purchase,
// immediate upgrade, one-off, ...) credits as a purchase.
func rxEventType(origin string) string {
	if origin == "renewal" {
		return "renewal"
	}
	return "purchase"
}

// rxTime parses Recurly's datetime text ("2014-01-01T20:24:02Z"; empty or
// nil="true" elements decode to "" and report !ok).
func rxTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (a *RecurlyAdapter) resolveAndPurchase(ctx context.Context, ev *commissions.Event, inv *rxInvoice) error {
	// Full ladder query (house pattern): with no token/coupon rungs for
	// Recurly this resolves via the customer binding.
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		CustomerExtID: ev.CustomerExtID,
		CampaignID:    ev.CampaignID,
		Now:           ev.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("recurly: attribution: %w", err)
	}
	if res.Method == "" && !res.Flagged {
		// Not bound yet: this is the account's first sale. Its token, if
		// the merchant set one, lives in the aff_token custom field.
		tok, err := a.accountToken(ctx, ev.CustomerExtID)
		if err != nil {
			return err // transient: Recurly retries the notification
		}
		if tok != "" {
			ev.ClickToken = tok
			if res, err = a.Resolver.Resolve(ctx, attribution.Query{
				CustomerExtID: ev.CustomerExtID, Token: tok, CampaignID: ev.CampaignID, Now: ev.OccurredAt,
			}); err != nil {
				return fmt.Errorf("recurly: attribution: %w", err)
			}
		}
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
	// Refund transactions include tax: they prorate against the invoice
	// total (the refund basis), not the tax-exclusive gross.
	ref := inv.InvoiceNumberPrefix + strconv.FormatInt(inv.InvoiceNumber, 10)
	cid, err := a.Engine.ProcessPurchase(ctx, *ev)
	switch {
	case errors.Is(err, commissions.ErrAlreadyProcessed):
		// Replay: success no-op, after finishing a link an earlier
		// delivery may have failed to write.
		return relinkReplay(ctx, a.Engine, a.Store, *ev, inv.TotalInCents, inv.UUID, ref)
	case errors.Is(err, commissions.ErrNoAttribution):
		// Organic sale: permanent condition — ACK so Recurly's ten
		// exponential retries stop (event recorded as "unattributed").
		log.Printf("billing: recurly/%s unattributed (recorded, skipped)", ev.ExternalID)
		return nil
	case err != nil:
		return err
	}
	return linkCommission(ctx, a.Store, *ev, cid, inv.TotalInCents, inv.UUID, ref)
}

// handleRefund maps refund/void transactions to clawbacks. kind prefixes
// the idempotency key (a void and a refund never share a transaction id,
// but the keys stay honest to what happened).
func (a *RecurlyAdapter) handleRefund(ctx context.Context, n rxNotification, kind string) error {
	tx := n.Transaction
	if tx == nil || tx.ID == "" {
		return fmt.Errorf("%w: recurly refund transaction missing", ErrMalformedEvent)
	}
	refunded := tx.AmountInCents
	if refunded <= 0 {
		return nil
	}
	ref := tx.InvoiceNumberPrefix + strconv.FormatInt(tx.InvoiceNumber, 10)
	commissionID, found, err := a.Store.FindCommission(ctx, "recurly", tx.InvoiceID, ref)
	if err != nil {
		return err
	}
	if !found {
		return a.Store.RecordRefundMiss(ctx, "recurly",
			kind+":"+tx.ID, []string{tx.InvoiceID, ref}, refunded, "")
	}
	// Proration base = the invoice total recorded at credit time (refund
	// transactions include tax, like it); older commissions, credited on
	// that total, use their gross. Legacy rows without gross fall back to
	// the commission amount, then to the refunded amount (full clawback).
	original := refundBase(ctx, a.Store, commissionID, refunded)
	occurred, ok := rxTime(tx.Date)
	if !ok {
		occurred = time.Now().UTC()
	}
	ev := commissions.Event{
		Source:     "recurly",
		ExternalID: kind + ":" + tx.ID,
		EventType:  "refund",
		OccurredAt: occurred,
	}
	err = a.Engine.ProcessRefund(ctx, ev, commissionID, refunded, original)
	if errors.Is(err, commissions.ErrAlreadyProcessed) {
		return nil // replayed refund notification: success no-op
	}
	return err
}

// accountToken reads the account's aff_token (raw at_token or its at1_
// form). No API key, no field or an unknown account = "". A refused key
// is logged, not retried: a retry cannot fix it.
func (a *RecurlyAdapter) accountToken(ctx context.Context, accountCode string) (string, error) {
	if a.AccountToken == nil || accountCode == "" {
		return "", nil
	}
	tok, err := a.AccountToken(ctx, accountCode)
	if errors.Is(err, ErrRecurlyAPIKey) {
		log.Printf("billing: recurly: AE_RECURLY_API_KEY was refused; account %s read without its aff_token (operator attention required)", accountCode)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("recurly: read aff_token for %s: %w", accountCode, err)
	}
	if v, ok := tokens.FromClientReferenceID(tok); ok {
		return v, nil
	}
	return tok, nil
}

// bind writes the lifetime customer binding from the account's aff_token
// without a credit. Already bound, no key, no token or no binder = nothing
// to do.
func (a *RecurlyAdapter) bind(ctx context.Context, accountCode string) error {
	binder, ok := a.Engine.(CustomerBinder)
	if accountCode == "" || a.AccountToken == nil || a.Resolver == nil || !ok {
		return nil
	}
	if res, err := a.Resolver.Resolve(ctx, attribution.Query{CustomerExtID: accountCode, CampaignID: 1, Now: time.Now().UTC()}); err == nil && res.Method == "customer" {
		return nil // bound already: no API call
	}
	tok, err := a.accountToken(ctx, accountCode)
	if err != nil || tok == "" {
		return err
	}
	res, err := a.Resolver.Resolve(ctx, attribution.Query{Token: tok, CampaignID: 1, Now: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("recurly: resolve aff_token: %w", err)
	}
	if res.Method != "token" {
		log.Printf("billing: recurly: aff_token for %s invalid, expired or affiliate not active (not bound)", accountCode)
		return nil
	}
	return binder.BindCustomer(ctx, accountCode, res.AffiliateID, res.CampaignID, "token")
}
