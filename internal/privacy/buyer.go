package privacy

// Buyer subject rights: a customer who bought through an affiliate link
// is a data subject too. The program owner can export or erase a buyer
// by email or by provider customer id. Erasure removes the buyer's
// personal details (email, the visit behind the purchase, fraud text
// quoting them) across the engine. The provider customer id stays: it is
// pseudonymous, like a payment id, and it is the key of the lifetime
// binding that keeps paying the affiliate on a live subscription.
// Commission rows keep every amount.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrUnknownBuyer is returned by ExportBuyer when nothing stored matches.
var ErrUnknownBuyer = errors.New("privacy: no stored data for this buyer")

// ErrNoBuyerRef is returned when neither an email nor a customer id is given.
var ErrNoBuyerRef = errors.New("privacy: a buyer email or customer id is required")

// BuyerRef names a buyer. Either field may be empty, not both.
type BuyerRef struct {
	Email      string
	CustomerID string
}

// buyerIdentity is every identifier the engine holds for one buyer: the
// given ones plus those linked to them (the customer ids on commissions
// and events with the buyer's email, and the reverse).
type buyerIdentity struct {
	emails      []string // lowercased
	customerIDs []string
}

func (b *buyerIdentity) addEmail(e string) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e != "" && !contains(b.emails, e) {
		b.emails = append(b.emails, e)
	}
}

func (b *buyerIdentity) addCustomer(id string) {
	id = strings.TrimSpace(id)
	if id != "" && !contains(b.customerIDs, id) {
		b.customerIDs = append(b.customerIDs, id)
	}
}

func contains(xs []string, x string) bool {
	for _, have := range xs {
		if have == x {
			return true
		}
	}
	return false
}

// matchSQL is the commissions predicate for this buyer, with its args.
func (b *buyerIdentity) matchSQL(alias string) (string, []any) {
	var parts []string
	var args []any
	for _, e := range b.emails {
		parts = append(parts, alias+"buyer_email != '' AND LOWER("+alias+"buyer_email) = ?")
		args = append(args, e)
	}
	for _, id := range b.customerIDs {
		parts = append(parts, alias+"customer_ext_id = ?")
		args = append(args, id)
	}
	if len(parts) == 0 {
		return "1 = 0", nil
	}
	return "((" + strings.Join(parts, ") OR (") + "))", args
}

// querier is the read surface shared by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// resolveBuyer expands ref through commissions and stored events until no
// new identifier appears (an email leads to customer ids and back).
func resolveBuyer(ctx context.Context, db querier, ref BuyerRef) (*buyerIdentity, error) {
	b := &buyerIdentity{}
	b.addEmail(ref.Email)
	b.addCustomer(ref.CustomerID)
	if len(b.emails) == 0 && len(b.customerIDs) == 0 {
		return nil, ErrNoBuyerRef
	}
	for range 3 { // email -> id -> email settles in two rounds
		before := len(b.emails) + len(b.customerIDs)
		where, args := b.matchSQL("")
		rows, err := db.QueryContext(ctx, `SELECT buyer_email, customer_ext_id FROM commissions WHERE `+where, args...)
		if err != nil {
			return nil, fmt.Errorf("privacy: buyer lookup: %w", err)
		}
		for rows.Next() {
			var e, id string
			if err := rows.Scan(&e, &id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("privacy: buyer lookup: %w", err)
			}
			b.addEmail(e)
			b.addCustomer(id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("privacy: buyer lookup: %w", err)
		}
		_ = rows.Close()
		if err := b.fromEvents(ctx, db); err != nil {
			return nil, err
		}
		if len(b.emails)+len(b.customerIDs) == before {
			break
		}
	}
	return b, nil
}

// fromEvents links identifiers through stored billing events: an event
// whose CustomerEmail is the buyer's names their CustomerExtID, and the
// reverse.
func (b *buyerIdentity) fromEvents(ctx context.Context, db querier) error {
	where, args := likeAny("payload", b.lookupPatterns())
	for _, table := range []string{"raw_events", "webhook_events"} {
		rows, err := db.QueryContext(ctx, `SELECT payload FROM `+table+` WHERE `+where, args...)
		if err != nil {
			return fmt.Errorf("privacy: buyer lookup %s: %w", table, err)
		}
		var found [][2]string
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				_ = rows.Close()
				return fmt.Errorf("privacy: buyer lookup %s: %w", table, err)
			}
			if e, id, ok := eventCustomer(payload); ok {
				found = append(found, [2]string{e, id})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("privacy: buyer lookup %s: %w", table, err)
		}
		_ = rows.Close()
		for _, f := range found {
			if b.owns(f[0], f[1]) {
				b.addEmail(f[0])
				b.addCustomer(f[1])
			}
		}
	}
	return nil
}

// eventCustomer reads the buyer fields of a stored engine Event (a parked
// invoice or held refund nests it under "event").
func eventCustomer(payload []byte) (email, customerID string, ok bool) {
	var doc map[string]any
	if json.Unmarshal(payload, &doc) != nil {
		return "", "", false
	}
	if ev, isMap := doc["event"].(map[string]any); isMap {
		doc = ev
	}
	email, _ = doc["CustomerEmail"].(string)
	customerID, _ = doc["CustomerExtID"].(string)
	return email, customerID, email != "" || customerID != ""
}

// owns reports whether an event's email or customer id is this buyer's.
func (b *buyerIdentity) owns(email, customerID string) bool {
	e, id := strings.ToLower(strings.TrimSpace(email)), strings.TrimSpace(customerID)
	return (e != "" && contains(b.emails, e)) || (id != "" && contains(b.customerIDs, id))
}

// scrubber builds the payload scrubber for this buyer: personal fields
// only. Customer ids are not redacted (see the package note above).
func (b *buyerIdentity) scrubber() *scrubber {
	sc := newScrubber()
	for _, e := range b.emails {
		sc.addEmail(e)
	}
	return sc
}

// lookupPatterns are the SQL prefilter patterns that find the buyer's
// stored events by email or by customer id.
func (b *buyerIdentity) lookupPatterns() []string {
	sc := b.scrubber()
	for _, id := range b.customerIDs {
		sc.addKeyed("customer", id)
	}
	return sc.prefilter()
}

// BuyerBundle is the data export for one buyer.
type BuyerBundle struct {
	GeneratedAt string           `json:"generated_at"`
	Subject     map[string]any   `json:"subject"`
	Purchases   []map[string]any `json:"purchases"`
	Customers   []map[string]any `json:"customers"`
	Clicks      []map[string]any `json:"clicks"`
	Events      []map[string]any `json:"events"`
	Counters    map[string]int64 `json:"counters"`
}

// JSON renders the bundle as the delivery artifact.
func (b *BuyerBundle) JSON() ([]byte, error) { return json.MarshalIndent(b, "", "  ") }

// ExportBuyer collects what the engine stores about one buyer. Commission
// amounts are the affiliate's earnings, not the buyer's data, so a
// purchase lists the order and its gross amount only.
func (s *Store) ExportBuyer(ctx context.Context, ref BuyerRef) (*BuyerBundle, error) {
	id, err := resolveBuyer(ctx, s.db, ref)
	if err != nil {
		return nil, err
	}
	sort.Strings(id.emails)
	sort.Strings(id.customerIDs)
	b := &BuyerBundle{
		GeneratedAt: s.now().UTC().Format(time.RFC3339),
		Subject:     map[string]any{"emails": id.emails, "customer_ids": id.customerIDs},
	}
	where, args := id.matchSQL("")
	if b.Purchases, err = s.many(ctx, `SELECT id, created_at, buyer_email, customer_ext_id, order_id,
		gross_minor, currency, country, coupon_code, attribution_method, status
		FROM commissions WHERE `+where+` ORDER BY id`, args...); err != nil {
		return nil, err
	}
	if len(id.customerIDs) > 0 {
		in, inArgs := placeholders(id.customerIDs)
		if b.Customers, err = s.many(ctx, `SELECT program_id, external_customer_id, bound_at, bound_method
			FROM customers WHERE external_customer_id IN (`+in+`) ORDER BY id`, inArgs...); err != nil {
			return nil, err
		}
	}
	whereC, argsC := id.matchSQL("c.")
	if b.Clicks, err = s.many(ctx, `SELECT k.id, k.ts, k.country, k.referer, k.landing_path, k.utm_source
		FROM clicks k WHERE k.id IN (SELECT c.click_id FROM commissions c WHERE c.click_id IS NOT NULL AND `+whereC+`)
		ORDER BY k.ts`, argsC...); err != nil {
		return nil, err
	}
	if b.Events, err = s.buyerEvents(ctx, id); err != nil {
		return nil, err
	}
	if len(b.Purchases) == 0 && len(b.Customers) == 0 && len(b.Clicks) == 0 && len(b.Events) == 0 {
		return nil, ErrUnknownBuyer
	}
	b.Counters = map[string]int64{
		"purchases": int64(len(b.Purchases)),
		"customers": int64(len(b.Customers)),
		"clicks":    int64(len(b.Clicks)),
		"events":    int64(len(b.Events)),
	}
	return b, nil
}

// buyerEvents lists the stored billing events that carry the buyer's
// details (metadata only: source, id, type, time).
func (s *Store) buyerEvents(ctx context.Context, id *buyerIdentity) ([]map[string]any, error) {
	sc := id.scrubber()
	where, args := likeAny("payload", id.lookupPatterns())
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, external_event_id, event_type, received_at, payload
		FROM raw_events WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var source, extID, evType string
		var rowID int64
		var received any
		var payload []byte
		if err := rows.Scan(&rowID, &source, &extID, &evType, &received, &payload); err != nil {
			return nil, err
		}
		// The LIKE prefilter matches substrings; keep only events that
		// really carry this buyer's email or customer id.
		e, cid, _ := eventCustomer(payload)
		if _, hasEmail := sc.scrubJSON(payload); !hasEmail && !id.owns(e, cid) {
			continue
		}
		out = append(out, rowMap(
			[]string{"id", "source", "external_event_id", "event_type", "received_at"},
			[]any{rowID, source, extID, evType, received}))
	}
	return out, rows.Err()
}

// BuyerEraseReport documents a buyer erasure.
type BuyerEraseReport struct {
	At            string           `json:"at"`
	Matched       map[string]int64 `json:"matched"`
	Scrubbed      map[string]int64 `json:"scrubbed"`
	Deleted       map[string]int64 `json:"deleted"`
	PreservedNote string           `json:"preserved_note"`
}

// Total is the number of rows the erase changed.
func (r *BuyerEraseReport) Total() int64 {
	var n int64
	for _, v := range r.Scrubbed {
		n += v
	}
	for _, v := range r.Deleted {
		n += v
	}
	return n
}

const buyerPreservedNote = "commission rows keep every amount, status and order id under the " +
	"affiliate's id; the provider customer id is kept everywhere (commissions, customer bindings, " +
	"stored events, usage_events and usage_daily, which hold only that id and unit counts): it is " +
	"pseudonymous and it is the lifetime binding key, so the affiliate keeps earning on a live " +
	"subscription; raw_events/webhook_events rows keep their event ids (the duplicate-delivery " +
	"guard) with the buyer's email removed from the payload; payment_claims keep provider payment " +
	"ids (pseudonymous, needed to stop double credit)"

// EraseBuyer removes one buyer's personal details across the engine:
// their email leaves commissions, stored events and fraud text, and the
// clicks that led to their purchases lose the visitor hashes and
// referer. The provider customer id and the lifetime binding stay (see
// buyerPreservedNote). Amounts are untouched. A second run changes
// nothing.
func (s *Store) EraseBuyer(ctx context.Context, ref BuyerRef) (*BuyerEraseReport, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("privacy: buyer erase begin: %w", err)
	}
	defer tx.Rollback()
	id, err := resolveBuyer(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	rep := &BuyerEraseReport{
		At:            s.now().UTC().Format(time.RFC3339),
		Matched:       map[string]int64{"emails": int64(len(id.emails)), "customer_ids": int64(len(id.customerIDs))},
		Scrubbed:      map[string]int64{},
		Deleted:       map[string]int64{},
		PreservedNote: buyerPreservedNote,
	}

	// 1) The clicks behind the buyer's purchases: the visitor was the
	// buyer. uniq_daily rows for those visitor hashes go first (they are
	// found through the click's hash).
	whereC, argsC := id.matchSQL("c.")
	clickIDs := `SELECT c.click_id FROM commissions c WHERE c.click_id IS NOT NULL AND ` + whereC
	if n, err := exec(tx, ctx, "uniq_daily", `DELETE FROM uniq_daily WHERE EXISTS (
		SELECT 1 FROM clicks k WHERE k.ip_hash IS NOT NULL AND k.ip_hash = uniq_daily.ip_hash
		  AND k.affiliate_id = uniq_daily.affiliate_id AND k.day = uniq_daily.day
		  AND k.id IN (`+clickIDs+`))`, argsC...); err != nil {
		return nil, err
	} else {
		rep.Deleted["uniq_daily"] = n
	}
	if n, err := exec(tx, ctx, "clicks", `UPDATE clicks SET
		ip_hash = NULL, ua_hash = NULL, referer = '', extra_json = '{}'
		WHERE id IN (`+clickIDs+`)
		  AND (ip_hash IS NOT NULL OR ua_hash IS NOT NULL OR referer != '' OR extra_json != '{}')`, argsC...); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["clicks"] = n
	}

	// 2) Fraud text quoting the buyer's email (self_referral quotes it).
	// Holds first: they are found through the commissions matched below.
	if len(id.emails) > 0 {
		var pats []string
		for _, e := range id.emails {
			pats = append(pats, emailResiduePatterns(e)...)
		}
		textLike := func(col string) (string, []any) {
			var parts []string
			var args []any
			for _, p := range pats {
				parts = append(parts, col+` LIKE ? ESCAPE '\'`)
				args = append(args, p)
			}
			return "(" + strings.Join(parts, " OR ") + ")", args
		}
		w, a := textLike("LOWER(reason)")
		if n, err := exec(tx, ctx, "fraud_verdicts", `UPDATE fraud_verdicts SET reason = '[erased]'
			WHERE reason != '[erased]' AND `+w, a...); err != nil {
			return nil, err
		} else {
			rep.Scrubbed["fraud_verdicts"] = n
		}
		wr, ar := textLike("LOWER(reason)")
		wn, an := textLike("LOWER(note)")
		if n, err := exec(tx, ctx, "commission_holds", `UPDATE commission_holds SET
			reason = CASE WHEN `+wr+` THEN '[erased]' ELSE reason END,
			note = CASE WHEN `+wn+` THEN '' ELSE note END
			WHERE (reason != '[erased]' AND `+wr+`) OR (note != '' AND `+wn+`)`,
			append(append(append(append([]any{}, ar...), an...), ar...), an...)...); err != nil {
			return nil, err
		} else {
			rep.Scrubbed["commission_holds"] = n
		}
	}

	// 3) Commission buyer email. Amounts, status, order id and the
	// provider customer id stay; the money-immutable triggers permit
	// this column.
	where, args := id.matchSQL("")
	if n, err := exec(tx, ctx, "commissions", `UPDATE commissions SET buyer_email = ''
		WHERE buyer_email != '' AND `+where, args...); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["commissions"] = n
	}

	// 4) Customer bindings and usage meters are kept: they hold only the
	// provider customer id (see buyerPreservedNote).

	// 5) Stored billing events and outbox rows.
	ns, err := id.scrubber().scrubPayloads(ctx, tx)
	if err != nil {
		return nil, err
	}
	for t, n := range ns {
		rep.Scrubbed[t] = n
	}

	// 6) Audit record, only when something changed. The detail carries
	// no identifier: the audit log is append-only.
	if rep.Total() > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audit_log (actor, table_name, row_id, action, detail) VALUES ('system', 'buyers', 0, 'erasure', ?)`,
			fmt.Sprintf("gdpr: buyer data erased; %d commissions anonymised; money rows preserved", rep.Scrubbed["commissions"])); err != nil {
			return nil, fmt.Errorf("privacy: buyer erase audit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("privacy: buyer erase commit: %w", err)
	}
	return rep, nil
}

// placeholders renders "?, ?, ?" and the args for an IN list.
func placeholders(vals []string) (string, []any) {
	ph := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		args[i] = v
	}
	return strings.Join(ph, ", "), args
}
