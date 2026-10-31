// linkage.go — refund linkage written at credit time: link keys, the
// refund basis, and Stripe payment aliases, plus the replay repair that
// keeps a failed link from stranding a commission.
package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// CreditedFinder is the optional engine capability that finds the
// commission a purchase event credited. *commissions.Engine and the
// screening decorator implement it; without it a replayed delivery cannot
// repair a missing refund link.
type CreditedFinder interface {
	CreditedCommission(ctx context.Context, source, externalID string) (int64, error)
}

var _ CreditedFinder = (*commissions.Engine)(nil)

// linkCommission writes refund linkage for a fresh credit: the keys a
// later refund carries, and the refund basis (the amount charged, tax
// included) for providers whose refunds are measured on it; 0 = none.
// Failure is returned, never swallowed: the webhook errors, the provider
// retries, and the retry relinks (relinkReplay). A dropped link left a
// commission no refund could ever claw.
func linkCommission(ctx context.Context, st Store, ev commissions.Event, cid, basis int64, keys ...string) error {
	if cid <= 0 || st == nil {
		return nil
	}
	if keys = compact(keys); len(keys) > 0 {
		if err := st.LinkCommission(ctx, ev.Source, ev.ExternalID, cid, keys...); err != nil {
			return fmt.Errorf("billing: link commission %d for %s/%s: %w", cid, ev.Source, ev.ExternalID, err)
		}
	}
	if s := sqlStore(st); s != nil && basis > 0 {
		if err := s.recordRefundBasis(ctx, ev.Source, cid, basis); err != nil {
			return err
		}
	}
	return nil
}

// relinkReplay finishes the linkage of an already-processed purchase: the
// engine ACKs the replay, but an earlier delivery may have credited and
// then failed to link. Every write is first-wins, so relinking a complete
// event changes nothing.
func relinkReplay(ctx context.Context, eng ProcessPurchaser, st Store, ev commissions.Event, basis int64, keys ...string) error {
	f, ok := eng.(CreditedFinder)
	if !ok || st == nil {
		return nil
	}
	cid, err := f.CreditedCommission(ctx, ev.Source, ev.ExternalID)
	if err != nil {
		return fmt.Errorf("billing: replay link lookup %s/%s: %w", ev.Source, ev.ExternalID, err)
	}
	return linkCommission(ctx, st, ev, cid, basis, keys...)
}

// sqlStore unwraps the SQL store behind st (tests wrap it); nil if none.
func sqlStore(st Store) *SQLStore {
	switch s := st.(type) {
	case *SQLStore:
		return s
	case interface{ unwrapSQL() *SQLStore }:
		return s.unwrapSQL()
	}
	return nil
}

func (s *SQLStore) unwrapSQL() *SQLStore { return s }

// exTax is the tax-exclusive part of a provider total: commission is paid
// on what the merchant keeps, never on tax collected for the state. A tax
// figure that is absent (0) leaves the total as is.
func exTax(total, tax int64) int64 {
	if tax <= 0 {
		return total
	}
	if tax >= total {
		return 0
	}
	return total - tax
}

// eventTime is when the provider says the event happened (RFC 3339 text,
// first one present), so a delivery retried after an outage is judged
// against the real sale time (token expiry, commission period, ledger
// date). Absent or unreadable: now.
func eventTime(stamps ...string) time.Time {
	for _, s := range stamps {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// unixTime is eventTime for providers that send Unix seconds.
func unixTime(sec int64) time.Time {
	if sec > 0 {
		return time.Unix(sec, 0).UTC()
	}
	return time.Now().UTC()
}

// put writes one keyed row (first write wins).
func (s *SQLStore) put(ctx context.Context, source, key string, p linkPayload) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("billing: marshal %s: %w", p.Kind, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
		VALUES (?, ?, TRUE, ?)
		ON CONFLICT(source, external_event_id) DO NOTHING`,
		source, key, string(payload)); err != nil {
		return fmt.Errorf("billing: insert %s/%s: %w", source, key, err)
	}
	return nil
}

// get reads one keyed row of the given kind ("" source = any source).
func (s *SQLStore) get(ctx context.Context, source, key, kind string) (linkPayload, bool, error) {
	var payload string
	var err error
	if source == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT payload FROM webhook_events WHERE external_event_id = ?`, key).Scan(&payload)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT payload FROM webhook_events WHERE source = ? AND external_event_id = ?`,
			source, key).Scan(&payload)
	}
	if err == sql.ErrNoRows {
		return linkPayload{}, false, nil
	}
	if err != nil {
		return linkPayload{}, false, fmt.Errorf("billing: lookup %s: %w", key, err)
	}
	var p linkPayload
	if json.Unmarshal([]byte(payload), &p) != nil || p.Kind != kind {
		return linkPayload{}, false, nil
	}
	return p, true, nil
}

// refundBasisKey is per commission (ids are global), so any refund that
// found the commission finds its basis.
func refundBasisKey(commissionID int64) string {
	return "refund_basis:" + strconv.FormatInt(commissionID, 10)
}

// recordRefundBasis keeps the amount a commission's refunds are measured
// against: the amount charged, tax included, where the commissionable
// gross excludes tax but the provider's refund amounts include it.
func (s *SQLStore) recordRefundBasis(ctx context.Context, source string, commissionID, basis int64) error {
	return s.put(ctx, source, refundBasisKey(commissionID),
		linkPayload{Kind: "refund_basis", CommissionID: commissionID, AmountMinor: basis})
}

func (s *SQLStore) refundBasis(ctx context.Context, commissionID int64) (int64, bool, error) {
	p, found, err := s.get(ctx, "", refundBasisKey(commissionID), "refund_basis")
	if err != nil || !found || p.AmountMinor <= 0 {
		return 0, false, err
	}
	return p.AmountMinor, true, nil
}

// aliasKey namespaces a payment alias apart from link keys.
func aliasKey(alias string) string { return "alias:" + alias }

// linkAlias records that each alias (a Stripe pi_/ch_) paid target (an
// in_). Since API 2025-03-31 neither the invoice nor the charge names the
// other; invoice_payment.paid is the only event that does, and Stripe does
// not order it against invoice.paid, so the alias is kept on its own and
// chased at refund time (findAlias).
func (s *SQLStore) linkAlias(ctx context.Context, source, target string, aliases ...string) error {
	for _, a := range compact(aliases) {
		if err := s.put(ctx, source, aliasKey(a), linkPayload{Kind: "alias", Keys: []string{target}}); err != nil {
			return err
		}
	}
	return nil
}

// findAlias returns the target recorded for the first alias that has one.
func (s *SQLStore) findAlias(ctx context.Context, source string, aliases ...string) (string, bool, error) {
	for _, a := range compact(aliases) {
		p, found, err := s.get(ctx, source, aliasKey(a), "alias")
		if err != nil {
			return "", false, err
		}
		if found && len(p.Keys) == 1 && p.Keys[0] != "" {
			return p.Keys[0], true, nil
		}
	}
	return "", false, nil
}
