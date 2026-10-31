// store.go — refund→commission linkage storage.
//
// Why a Store at all: a charge.refunded webhook carries the CHARGE (ch_),
// payment_intent (pi_), or invoice (in_) id — but the commission was
// idempotency-keyed on the original EVENT id (evt_...), which the refund
// object does not reference. So at credit time we record aliases
// (cs_/in_/pi_) → commission_id, and at refund time we look the parent up
// by any of the ids the charge object gives us.
//
// SQLStore persists aliases in the existing webhook_events table (unique on
// (source, external_event_id)) using external_event_id as the alias key and
// payload as a small JSON envelope. This reuses the schema without a
// migration; a dedicated link table can replace it behind the same
// interface later without touching the adapter. Unattributed purchases
// parked for a late credit ("parked:" keys) live in the same table.
package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// Store records and resolves commission linkage keys (provider object ids)
// for refund attribution. All methods must be safe for concurrent use.
type Store interface {
	// LinkCommission records that commissionID was credited for the given
	// provider object ids (keys). Implementations must tolerate duplicate
	// keys (first link wins). An error fails the webhook so the provider
	// retries, and the retry links again (linkCommission, relinkReplay).
	LinkCommission(ctx context.Context, source, eventID string, commissionID int64, keys ...string) error

	// FindCommission returns the commission linked to any key, trying keys
	// in order. found=false (not an error) means no commission exists for
	// any key — e.g. the sale predates linkage or was never commissionable.
	FindCommission(ctx context.Context, source string, keys ...string) (commissionID int64, found bool, err error)

	// RecordRefundMiss persists a refund that found no parent commission,
	// for reconciliation. Best-effort.
	RecordRefundMiss(ctx context.Context, source, refundEventID string, keys []string, amountMinor int64, currency string) error
}

// linkPayload is the JSON envelope stored in webhook_events.payload.
type linkPayload struct {
	Kind         string   `json:"kind"` // "link" | "refund_miss"
	EventID      string   `json:"event_id,omitempty"`
	CommissionID int64    `json:"commission_id,omitempty"`
	Keys         []string `json:"keys,omitempty"`
	AmountMinor  int64    `json:"amount_minor,omitempty"`
	Currency     string   `json:"currency,omitempty"`
}

// SQLStore is the stdlib SQL implementation of Store (SQLite + PG
// compatible: only INSERT ... ON CONFLICT DO NOTHING and SELECTs).
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore binds a store to the general pool handle.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// LinkCommission implements Store. Duplicate alias keys are ignored
// (first commission to claim a key owns it) via ON CONFLICT DO NOTHING.
func (s *SQLStore) LinkCommission(ctx context.Context, source, eventID string, commissionID int64, keys ...string) error {
	for _, k := range compact(keys) {
		payload, err := json.Marshal(linkPayload{
			Kind: "link", EventID: eventID, CommissionID: commissionID, Keys: compact(keys),
		})
		if err != nil {
			return fmt.Errorf("billing: marshal link payload: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
			VALUES (?, ?, TRUE, ?)
			ON CONFLICT(source, external_event_id) DO NOTHING`,
			source, k, string(payload)); err != nil {
			return fmt.Errorf("billing: insert link %s/%s: %w", source, k, err)
		}
	}
	return nil
}

// FindCommission implements Store: first key (in order) with a row wins.
func (s *SQLStore) FindCommission(ctx context.Context, source string, keys ...string) (int64, bool, error) {
	for _, k := range compact(keys) {
		var payload string
		err := s.db.QueryRowContext(ctx,
			`SELECT payload FROM webhook_events WHERE source = ? AND external_event_id = ?`,
			source, k).Scan(&payload)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, false, fmt.Errorf("billing: link lookup %s/%s: %w", source, k, err)
		}
		var p linkPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue // not ours / corrupt: keep looking
		}
		if p.Kind == "link" && p.CommissionID > 0 {
			return p.CommissionID, true, nil
		}
	}
	// No link row: the payment claim the credit wrote in its own
	// transaction (commissions.Event.PaymentKeys) still names the
	// commission, even when the link write after it failed.
	for _, k := range compact(keys) {
		var cid int64
		err := s.db.QueryRowContext(ctx,
			`SELECT commission_id FROM payment_claims WHERE source = ? AND payment_key = ?`,
			source, k).Scan(&cid)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, false, fmt.Errorf("billing: payment claim lookup %s/%s: %w", source, k, err)
		}
		if cid > 0 {
			return cid, true, nil
		}
	}
	return 0, false, nil
}

// refundMissKey namespaces miss records so they cannot collide with alias
// keys (provider ids never contain ':').
func refundMissKey(refundEventID string) string { return "refund_miss:" + refundEventID }

// RecordRefundMiss implements Store.
func (s *SQLStore) RecordRefundMiss(ctx context.Context, source, refundEventID string, keys []string, amountMinor int64, currency string) error {
	payload, err := json.Marshal(linkPayload{
		Kind: "refund_miss", EventID: refundEventID, Keys: compact(keys),
		AmountMinor: amountMinor, Currency: currency,
	})
	if err != nil {
		return fmt.Errorf("billing: marshal refund miss: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
		VALUES (?, ?, TRUE, ?)
		ON CONFLICT(source, external_event_id) DO NOTHING`,
		source, refundMissKey(refundEventID), string(payload)); err != nil {
		return fmt.Errorf("billing: insert refund miss %s: %w", refundEventID, err)
	}
	return nil
}

// parked is a purchase the engine recorded as unattributed, kept so a later
// event carrying the buyer's attribution can credit it (see
// StripeAdapter.parkFirstInvoice).
type parked struct {
	Kind     string            `json:"kind"` // "parked"
	Event    commissions.Event `json:"event"`
	LinkKeys []string          `json:"link_keys,omitempty"` // refund linkage for the late credit
	Basis    int64             `json:"basis,omitempty"`     // amount charged (refund basis)
}

// parkStore is the optional Store capability behind late attribution.
// SQLStore implements it; without it nothing is parked (a late signal
// then binds future sales only).
type parkStore interface {
	park(ctx context.Context, source string, p parked, keys ...string) error
	findParked(ctx context.Context, source string, keys ...string) (parked, bool, error)
}

var _ parkStore = (*SQLStore)(nil)

// parkedKey namespaces parked rows apart from alias keys and refund misses.
func parkedKey(k string) string { return "parked:" + k }

// park records p under each key (first write wins). Rows are never
// updated or deleted: a late credit dedupes on its own raw_events key.
func (s *SQLStore) park(ctx context.Context, source string, p parked, keys ...string) error {
	p.Kind = "parked"
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("billing: marshal parked: %w", err)
	}
	for _, k := range compact(keys) {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
			VALUES (?, ?, TRUE, ?)
			ON CONFLICT(source, external_event_id) DO NOTHING`,
			source, parkedKey(k), string(payload)); err != nil {
			return fmt.Errorf("billing: park %s/%s: %w", source, k, err)
		}
	}
	return nil
}

// findParked returns the purchase parked under the first key that has one.
func (s *SQLStore) findParked(ctx context.Context, source string, keys ...string) (parked, bool, error) {
	for _, k := range compact(keys) {
		var payload string
		err := s.db.QueryRowContext(ctx,
			`SELECT payload FROM webhook_events WHERE source = ? AND external_event_id = ?`,
			source, parkedKey(k)).Scan(&payload)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return parked{}, false, fmt.Errorf("billing: parked lookup %s/%s: %w", source, k, err)
		}
		var p parked
		if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Kind != "parked" {
			continue
		}
		return p, true, nil
	}
	return parked{}, false, nil
}

// refundBase returns the proration denominator for a refund clawing back
// commissionID: the SALE gross (commissions.gross_minor) the commission was
// computed from. Provider refund amounts are sale money, so the clawback
// ratio must be refunded/gross. Using the commission amount as the base
// inflated the ratio by 1/rate (a 25% refund on a 10% commission clawed
// 100%). Legacy rows (gross_minor 0/NULL) fall back to the commission
// amount (the previous behavior), then to fallback. Only SQLStore can
// answer; any other Store gets fallback.
//
// A refund basis recorded at credit time (recordRefundBasis: the amount
// charged, tax included) wins over the gross: it is what tax-inclusive
// refund amounts must be measured against.
func refundBase(ctx context.Context, st Store, commissionID, fallback int64) int64 {
	s := sqlStore(st)
	if s == nil {
		return fallback
	}
	if b, found, err := s.refundBasis(ctx, commissionID); err == nil && found {
		return b
	}
	var gross, amount sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT gross_minor, amount FROM commissions WHERE id = ?`, commissionID).Scan(&gross, &amount); err != nil {
		return fallback
	}
	if gross.Valid && gross.Int64 > 0 {
		return gross.Int64
	}
	if amount.Valid && amount.Int64 > 0 {
		return amount.Int64
	}
	return fallback
}
