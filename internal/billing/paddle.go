// Paddle Billing webhook adapter — the second adapter, proving the billing
// Adapter interface generalizes (FR-16: normalized event adapters).
//
// Signature scheme (Paddle Billing): header
// "Paddle-Signature: ts=...;h1=...[;h1=...]" where
// h1 = hex(HMAC-SHA256(webhook_secret, ts + ":" + rawBody)). During secret
// rotation Paddle sends one h1 per active secret; any match verifies.
// Verification precedes any DB write (same ordering contract as Stripe).
//
// Money events:
//
//	transaction.completed          -> purchase (ONLY this status: .ready/.paid/
//	                                  .billed fire before or alongside it for
//	                                  the same transaction)
//	adjustment.created / .updated  -> refund/chargeback clawback, ONLY once the
//	                                  adjustment is approved
//
// Idempotency keys: the purchase is keyed on the TRANSACTION id (txn_...),
// not the delivery's event id, so a re-sent notification or a second
// completed event for the same transaction dedupes in raw_events. Clawbacks
// are keyed on the adjustment id (adj_...) so created+updated for one
// adjustment claw once.
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

// PaddleSignatureTolerance mirrors Stripe's 5-minute window.
const PaddleSignatureTolerance = 5 * time.Minute

// PaddleAdapter verifies and maps Paddle Billing webhooks.
type PaddleAdapter struct {
	Secret   string // Paddle webhook secret (whsec_-style, from the dashboard)
	Engine   ProcessPurchaser
	Resolver AttributionResolver
	Store    Store
}

// NewPaddleAdapter builds one.
func NewPaddleAdapter(secret string, engine ProcessPurchaser, resolver AttributionResolver, store Store) *PaddleAdapter {
	return &PaddleAdapter{Secret: secret, Engine: engine, Resolver: resolver, Store: store}
}

// paddleEnvelope is the Paddle event wrapper.
type paddleEnvelope struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	// OccurredAt is when the event happened (RFC 3339); retries keep it.
	OccurredAt string          `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// paddleTotals is a Paddle totals block (minor units as strings). total
// is after discount and includes tax; tax is the tax part of it.
type paddleTotals struct {
	Total      string `json:"total"`
	Tax        string `json:"tax"`
	GrandTotal string `json:"grand_total"`
}

// paddleCustomData is custom_data with every scalar kept as text. Paddle
// passes through whatever JSON the merchant set; decoding it as strings
// only made a number or a boolean fail the whole webhook, on every retry.
// Nested objects, arrays and nulls are ignored.
type paddleCustomData map[string]string

func (c *paddleCustomData) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		if string(b) == "null" {
			*c = nil
			return nil
		}
		return err
	}
	out := make(paddleCustomData, len(raw))
	for k, v := range raw {
		var sv string
		if json.Unmarshal(v, &sv) == nil {
			out[k] = sv
			continue
		}
		var n json.Number
		dec := json.NewDecoder(strings.NewReader(string(v)))
		dec.UseNumber()
		if dec.Decode(&n) == nil {
			out[k] = n.String()
			continue
		}
		var bv bool
		if json.Unmarshal(v, &bv) == nil {
			out[k] = strconv.FormatBool(bv)
		}
	}
	*c = out
	return nil
}

// paddleTransaction is a transaction.completed payload.
type paddleTransaction struct {
	ID            string `json:"id"`
	CustomerID    string `json:"customer_id"`
	CustomerEmail string `json:"customer_email"`
	CurrencyCode  string `json:"currency_code"`
	// Paddle Billing carries the calculated totals under details.totals.
	Details struct {
		Totals paddleTotals `json:"totals"`
	} `json:"details"`
	// Legacy/flattened top-level totals: fallback only.
	Totals paddleTotals `json:"totals"`
	Items  []struct {
		Price struct {
			ID        string `json:"id"`
			UnitPrice string `json:"unit_price"`
		} `json:"price"`
		Quantity int `json:"quantity"`
	} `json:"items"`
	CustomData     paddleCustomData `json:"custom_data"`
	BillingDetails struct{}         `json:"billing_details"`
}

// paddleAdjustment is an adjustment.created/.updated payload
// (refund/credit/chargeback).
type paddleAdjustment struct {
	ID            string `json:"id"`
	TransactionID string `json:"transaction_id"`
	Action        string `json:"action"` // "refund" | "credit" | "chargeback" | ...
	Status        string `json:"status"` // "pending_approval" | "approved" | "rejected" | "reversed"
	CurrencyCode  string `json:"currency_code"`
	Totals        struct {
		Total string `json:"total"` // minor units as string (sign varies by action)
		Tax   string `json:"tax"`
	} `json:"totals"`
}

// HandleWebhook implements Adapter.
func (a *PaddleAdapter) HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error {
	// 1) Signature FIRST — before any parse or DB write.
	if err := a.verifySignature(rawBody, headers.Get("Paddle-Signature")); err != nil {
		return err
	}

	// 2) Envelope (signed raw body: duplicate-key divergence impossible —
	// the signature covers the exact bytes being parsed).
	var env paddleEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return fmt.Errorf("%w: paddle envelope: %v", ErrMalformedEvent, err)
	}

	switch env.EventType {
	case "transaction.completed":
		// Only the completed state is money that landed. transaction.ready
		// fires when a checkout is merely ready for payment (and .paid /
		// .billed fire for the same transaction) — crediting those paid for
		// abandoned checkouts and double-credited completed ones.
		return a.handleTransaction(ctx, env)
	case "adjustment.created", "adjustment.updated":
		return a.handleAdjustment(ctx, env)
	default:
		// Unknown types: success no-op (forward-compatible; the engine's
		// raw_events will dedupe if it ever becomes commissionable).
		return nil
	}
}

// customDataProductKey returns the per-product rate key for a Paddle
// transaction: explicit custom-data pin (aff_product > product, mirroring
// the Stripe aff_token/token convention) wins; else the first line item's
// price id (the plan purchased).
func customDataProductKey(custom map[string]string, items []struct {
	Price struct {
		ID        string `json:"id"`
		UnitPrice string `json:"unit_price"`
	} `json:"price"`
	Quantity int `json:"quantity"`
},
) string {
	if v := custom["aff_product"]; v != "" {
		return v
	}
	if v := custom["product"]; v != "" {
		return v
	}
	if len(items) > 0 {
		return items[0].Price.ID
	}
	return ""
}

// paddleTransactionGross returns the commissionable total in minor units:
// after discount, before tax (total - tax), like Stripe's
// total_excluding_tax. details.totals (Paddle Billing) first, the top-level
// block as a fallback. A block without a tax figure falls back to
// grand_total, then total.
func paddleTransactionGross(tx paddleTransaction) int64 {
	for _, t := range []paddleTotals{tx.Details.Totals, tx.Totals} {
		if v, ok := paddleExTax(t.Total, t.Tax); ok {
			return v
		}
		for _, s := range []string{t.GrandTotal, t.Total} {
			if strings.TrimSpace(s) == "" {
				continue
			}
			v, err := parsePaddleAmount(s)
			if err != nil {
				return 0 // present but unparsable: never guess money
			}
			return v
		}
	}
	return 0
}

// paddleExTax is total - tax when both are present and parse. Signs are
// kept, so it works for refund adjustments (both negative) too.
func paddleExTax(total, tax string) (int64, bool) {
	if strings.TrimSpace(total) == "" || strings.TrimSpace(tax) == "" {
		return 0, false
	}
	tv, err1 := parsePaddleAmount(total)
	xv, err2 := parsePaddleAmount(tax)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return tv - xv, true
}

func (a *PaddleAdapter) handleTransaction(ctx context.Context, env paddleEnvelope) error {
	var tx paddleTransaction
	if err := json.Unmarshal(env.Data, &tx); err != nil {
		return fmt.Errorf("%w: paddle transaction: %v", ErrMalformedEvent, err)
	}
	if tx.ID == "" {
		return fmt.Errorf("%w: paddle transaction id missing", ErrMalformedEvent)
	}
	amount := paddleTransactionGross(tx)
	if amount <= 0 {
		return nil // zero-total (100% discount) or unparsable: not commissionable
	}

	ev := commissions.Event{
		Source: "paddle",
		// Idempotency on the TRANSACTION, not the delivery: Paddle can
		// emit more than one notification (distinct event_ids) for the
		// same transaction; each must dedupe to one credit.
		ExternalID:    tx.ID,
		EventType:     "purchase",
		AmountMinor:   amount,
		Currency:      strings.ToUpper(tx.CurrencyCode),
		CustomerExtID: tx.CustomerID,
		CustomerEmail: tx.CustomerEmail,
		ClickToken:    tx.CustomData["token"],
		CouponCode:    tx.CustomData["coupon"],
		// Per-product rates (FR-12): explicit custom-data pin wins; else
		// the first line item's price id (the plan purchased).
		ProductKey: customDataProductKey(tx.CustomData, tx.Items),
		CampaignID: 1,
		OccurredAt: eventTime(env.OccurredAt),
		// Postback {order_id}: the Paddle transaction id.
		OrderID: tx.ID,
	}
	// Link the commission to both the event id and the TRANSACTION id —
	// refunds arrive carrying the transaction id. No refund basis: Paddle
	// refunds are measured tax-exclusive, like the sale.
	return a.resolveAndPurchase(ctx, &ev, env.EventID, tx.ID)
}

func (a *PaddleAdapter) handleAdjustment(ctx context.Context, env paddleEnvelope) error {
	var adj paddleAdjustment
	if err := json.Unmarshal(env.Data, &adj); err != nil {
		return fmt.Errorf("%w: paddle adjustment: %v", ErrMalformedEvent, err)
	}
	if adj.Action != "refund" && adj.Action != "chargeback" {
		return nil // credit/other adjustments: not commissionable
	}
	// Refunds are requested as pending_approval and may be rejected; only
	// an approved adjustment moved money back. Clawing on created-pending
	// let a buyer request-then-cancel a refund to wipe a commission.
	if adj.Status != "approved" {
		log.Printf("billing: paddle %s: adjustment %s status=%q (not approved, skipped)",
			env.EventID, adj.ID, adj.Status)
		return nil
	}
	// Refunded amount on the same basis as the sale: tax excluded when
	// Paddle states it, so a full refund claws the full commission.
	refunded, ok := paddleExTax(adj.Totals.Total, adj.Totals.Tax)
	if !ok {
		refunded, _ = parsePaddleAmount(adj.Totals.Total)
	}
	if refunded < 0 {
		refunded = -refunded
	}
	if refunded == 0 {
		return nil
	}
	// One clawback per adjustment: created(approved) and a later
	// updated(approved) for the same adjustment share this key.
	refundKey := adj.ID
	if refundKey == "" {
		refundKey = env.EventID
	}
	currency := strings.ToUpper(adj.CurrencyCode)
	if currency == "" {
		currency = "USD"
	}

	// Refund attribution: the purchase linked the commission to both the
	// event id AND the transaction id (keys). Try the transaction id — the
	// adjustment carries it directly.
	ev := commissions.Event{
		Source: "paddle", ExternalID: refundKey, EventType: "refund",
		Currency: currency, OccurredAt: eventTime(env.OccurredAt),
	}
	commissionID, found, err := a.Store.FindCommission(ctx, "paddle", adj.TransactionID)
	if err != nil {
		return err
	}
	if !found {
		if err := a.Store.RecordRefundMiss(ctx, "paddle", refundKey,
			[]string{adj.TransactionID}, refunded, currency); err != nil {
			return err
		}
		// The purchase may still come: it applies the held refund.
		return holdMissedRefund(ctx, a.Engine, a.Store, heldRefund{Event: ev, Refunded: refunded}, adj.TransactionID)
	}
	// Proration base = the SALE gross the commission was computed from
	// (refunded is sale money). Legacy rows without gross fall back to the
	// commission amount, then to the refunded amount (full clawback).
	originalAmount := refundBase(ctx, a.Store, commissionID, refunded)

	err = a.Engine.ProcessRefund(ctx, ev, commissionID, refunded, originalAmount)
	if errors.Is(err, commissions.ErrAlreadyProcessed) {
		return nil // replayed adjustment: success no-op
	}
	return err
}

// resolveAndPurchase: attribution pre-resolve then purchase (Stripe
// pattern), then refund linkage to keys.
func (a *PaddleAdapter) resolveAndPurchase(ctx context.Context, ev *commissions.Event, keys ...string) error {
	res, err := a.Resolver.Resolve(ctx, attribution.Query{
		CouponCode:    ev.CouponCode,
		CustomerExtID: ev.CustomerExtID,
		Token:         ev.ClickToken,
		CampaignID:    ev.CampaignID,
		Now:           ev.OccurredAt,
	})
	if err != nil {
		return fmt.Errorf("paddle: attribution: %w", err)
	}
	if res.Flagged {
		// Conservative default: flagged heuristic conversions skipped.
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
		return relinkReplay(ctx, a.Engine, a.Store, *ev, 0, keys...)
	case errors.Is(err, commissions.ErrNoAttribution):
		// Organic sale: permanent condition — ACK so the provider stops
		// retrying (the event is recorded in raw_events as "unattributed").
		log.Printf("billing: paddle/%s unattributed (recorded, skipped)", ev.ExternalID)
		return nil
	case err != nil:
		return err
	}
	return linkCommission(ctx, a.Engine, a.Store, *ev, cid, 0, keys...)
}

// verifySignature checks ts;h1=[;h1=...] per Paddle Billing spec. Every h1
// is collected (secret rotation sends one per active secret) and the
// delivery verifies when ANY of them matches, compared timing-safe.
func (a *PaddleAdapter) verifySignature(rawBody []byte, header string) error {
	if header == "" {
		return fmt.Errorf("%w: missing Paddle-Signature", ErrInvalidSignature)
	}
	var ts string
	var h1s []string
	for _, part := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ts":
			ts = kv[1]
		case "h1":
			if kv[1] != "" {
				h1s = append(h1s, kv[1])
			}
		}
	}
	if ts == "" || len(h1s) == 0 {
		return fmt.Errorf("%w: malformed Paddle-Signature", ErrInvalidSignature)
	}
	tsVal, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad ts", ErrInvalidSignature)
	}
	// Freshness window.
	age := time.Since(time.Unix(tsVal, 0))
	if age < -PaddleSignatureTolerance || age > PaddleSignatureTolerance {
		return fmt.Errorf("%w: stale ts", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write([]byte(ts + ":"))
	mac.Write(rawBody)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, h1 := range h1s {
		if hmac.Equal([]byte(want), []byte(strings.ToLower(h1))) {
			return nil
		}
	}
	return fmt.Errorf("%w: h1 mismatch", ErrInvalidSignature)
}

// parsePaddleAmount parses Paddle's string minor-unit amounts ("-1200", "4900").
func parsePaddleAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	return strconv.ParseInt(s, 10, 64)
}
