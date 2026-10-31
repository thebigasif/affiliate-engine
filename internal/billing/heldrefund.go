// heldrefund.go — refunds that arrive before their purchase.
//
// Providers do not order webhooks, so a refund can land before the
// purchase it refunds. It then finds no commission: it is recorded as a
// miss (reconciliation) and ACKed. Before this, nothing read the miss
// again, so the commission was later credited and paid in full.
//
// Now the refund is also held under each key it carries. When a credit
// links its keys (linkCommission), it applies every refund held under
// them through the same engine entry point the refund would have used.
// The engine dedupes refunds on their own event id (raw_events), so a
// held refund applied twice, or applied and then redelivered, claws once.
// An applied marker spares the engine call on later replays.
package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/money"
)

// heldRefund is everything needed to replay a refund once its commission
// exists.
type heldRefund struct {
	Kind     string            `json:"kind"` // "held_refund"
	Event    commissions.Event `json:"event"`
	Refunded int64             `json:"refunded"`
	// Original is the proration base the refund carried; 0 means the
	// commission's refundBase, read when it is applied.
	Original int64 `json:"original,omitempty"`
	// Cumulative: Refunded is the provider's running total
	// (ProcessRefundCumulative), not this refund's own amount.
	Cumulative bool `json:"cumulative,omitempty"`
}

// maxHeldPerKey bounds the slots probed under one key. A payment is
// refunded a handful of times at most; past the bound the refund stays a
// recorded miss only.
const maxHeldPerKey = 64

// heldKey is the n-th slot under a link key. Slots are probed in order,
// so every lookup is an exact match on the unique index (no LIKE scan).
func heldKey(key string, n int) string {
	return "held_refund:" + key + "#" + strconv.Itoa(n)
}

// heldAppliedKey marks a held refund as applied (or skipped for good).
func heldAppliedKey(refundEventID string) string { return "held_refund_done:" + refundEventID }

// holdRefund stores h under each key, in the first free slot. A refund
// already held under a key (a replayed delivery) is not stored again.
func (s *SQLStore) holdRefund(ctx context.Context, h heldRefund, keys ...string) error {
	h.Kind = "held_refund"
	payload, err := marshalPayload(h)
	if err != nil {
		return err
	}
	source := h.Event.Source
	for _, k := range compact(keys) {
		for n := 0; ; n++ {
			if n == maxHeldPerKey {
				log.Printf("billing: refund %s/%s: %d refunds already held under %s, not held", source, h.Event.ExternalID, n, k)
				break
			}
			res, err := s.db.ExecContext(ctx, `
				INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
				VALUES (?, ?, TRUE, ?)
				ON CONFLICT(source, external_event_id) DO NOTHING`,
				source, heldKey(k, n), payload)
			if err != nil {
				return fmt.Errorf("billing: hold refund %s/%s: %w", source, h.Event.ExternalID, err)
			}
			if c, err := res.RowsAffected(); err == nil && c == 1 {
				break
			}
			other, found, err := s.heldAt(ctx, source, k, n)
			if err != nil {
				return err
			}
			if found && other.Event.ExternalID == h.Event.ExternalID {
				break // already held here
			}
		}
	}
	return nil
}

// heldAt reads one slot (found=false when empty or not a held refund).
func (s *SQLStore) heldAt(ctx context.Context, source, key string, n int) (heldRefund, bool, error) {
	var h heldRefund
	found, err := s.getJSON(ctx, source, heldKey(key, n), &h)
	if err != nil || !found || h.Kind != "held_refund" {
		return heldRefund{}, false, err
	}
	return h, true, nil
}

// heldUnder lists the refunds held under keys, each once.
func (s *SQLStore) heldUnder(ctx context.Context, source string, keys ...string) ([]heldRefund, error) {
	var out []heldRefund
	seen := map[string]bool{}
	for _, k := range compact(keys) {
		for n := 0; n < maxHeldPerKey; n++ {
			var h heldRefund
			found, err := s.getJSON(ctx, source, heldKey(k, n), &h)
			if err != nil {
				return nil, err
			}
			if !found {
				break // slots fill in order: the first empty one ends the list
			}
			if h.Kind != "held_refund" || seen[h.Event.ExternalID] {
				continue
			}
			seen[h.Event.ExternalID] = true
			out = append(out, h)
		}
	}
	return out, nil
}

// copyHeld holds every refund held under from also under to. A Stripe
// refund held under a pi_/ch_ reaches the in_ the invoice credit links
// once invoice_payment.paid names that invoice.
func (s *SQLStore) copyHeld(ctx context.Context, source, to string, from ...string) error {
	held, err := s.heldUnder(ctx, source, from...)
	if err != nil {
		return err
	}
	for _, h := range held {
		if err := s.holdRefund(ctx, h, to); err != nil {
			return err
		}
	}
	return nil
}

// holdMissedRefund holds a refund that found no commission, so the
// purchase applies it when it links keys. A purchase that linked while
// this refund was looking would have missed the hold: the lookup is
// repeated after it, and a commission found then gets the refund now.
func holdMissedRefund(ctx context.Context, eng ProcessPurchaser, st Store, h heldRefund, keys ...string) error {
	s := sqlStore(st)
	if s == nil || eng == nil {
		return nil
	}
	if err := s.holdRefund(ctx, h, keys...); err != nil {
		return err
	}
	cid, found, err := st.FindCommission(ctx, h.Event.Source, keys...)
	if err != nil || !found {
		return err
	}
	return applyHeldRefunds(ctx, eng, s, h.Event.Source, cid, keys...)
}

// applyHeldRefunds claws commissionID for every refund held under keys
// and not yet applied. A refund in another currency than the commission
// is skipped (logged): its amounts cannot prorate the commission. An
// engine failure is returned, so the purchase webhook errors and the
// provider's retry applies it (relinkReplay).
func applyHeldRefunds(ctx context.Context, eng ProcessPurchaser, s *SQLStore, source string, commissionID int64, keys ...string) error {
	if eng == nil || commissionID <= 0 {
		return nil
	}
	held, err := s.heldUnder(ctx, source, keys...)
	if err != nil || len(held) == 0 {
		return err
	}
	var commissionCurrency string
	if err := s.db.QueryRowContext(ctx,
		`SELECT currency FROM commissions WHERE id = ?`, commissionID).Scan(&commissionCurrency); err != nil {
		return fmt.Errorf("billing: held refunds for commission %d: currency: %w", commissionID, err)
	}
	for _, h := range held {
		done := heldAppliedKey(h.Event.ExternalID)
		if _, applied, err := s.get(ctx, source, done, "held_refund_done"); err != nil {
			return err
		} else if applied {
			continue
		}
		if h.Event.Currency != "" && money.Normalize(h.Event.Currency) != money.Normalize(commissionCurrency) {
			log.Printf("billing: held refund %s/%s is in %s, commission %d in %s (skipped)",
				source, h.Event.ExternalID, h.Event.Currency, commissionID, commissionCurrency)
			if err := s.put(ctx, source, done, linkPayload{Kind: "held_refund_done", CommissionID: commissionID}); err != nil {
				return err
			}
			continue
		}
		original := h.Original
		if original <= 0 {
			original = refundBase(ctx, s, commissionID, h.Refunded)
		}
		if cr, ok := eng.(CumulativeRefunder); ok && h.Cumulative {
			err = cr.ProcessRefundCumulative(ctx, h.Event, commissionID, h.Refunded, original)
		} else {
			err = eng.ProcessRefund(ctx, h.Event, commissionID, h.Refunded, original)
		}
		if err != nil && !errors.Is(err, commissions.ErrAlreadyProcessed) {
			return fmt.Errorf("billing: apply held refund %s/%s to commission %d: %w",
				source, h.Event.ExternalID, commissionID, err)
		}
		log.Printf("billing: held refund %s/%s applied to commission %d", source, h.Event.ExternalID, commissionID)
		if err := s.put(ctx, source, done, linkPayload{Kind: "held_refund_done", CommissionID: commissionID}); err != nil {
			return err
		}
	}
	return nil
}

func marshalPayload(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("billing: marshal payload: %w", err)
	}
	return string(b), nil
}

// getJSON decodes one keyed row into v. found=false when the row is
// missing or not valid JSON.
func (s *SQLStore) getJSON(ctx context.Context, source, key string, v any) (bool, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM webhook_events WHERE source = ? AND external_event_id = ?`,
		source, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("billing: lookup %s: %w", key, err)
	}
	return json.Unmarshal([]byte(payload), v) == nil, nil
}
