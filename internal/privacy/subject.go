package privacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// ErrUnknownAffiliate is returned by Export/Erase for a nonexistent id.
var ErrUnknownAffiliate = errors.New("privacy: unknown affiliate")

// Store runs subject-rights operations against the repo handle.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore builds the subject-rights store.
func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

// ExportBundle is the machine-readable GDPR Art. 15/20 data export for
// one data subject (the affiliate). Money rows carry no PII beyond ids
// (by design), so the bundle is identity + activity + earnings facts.
type ExportBundle struct {
	GeneratedAt  string           `json:"generated_at"`
	Subject      map[string]any   `json:"subject"`
	Application  map[string]any   `json:"application"`
	Clicks       []map[string]any `json:"clicks"`
	Attributions []map[string]any `json:"attributions"`
	Customers    []map[string]any `json:"customers"`
	Commissions  []map[string]any `json:"commissions"`
	Payouts      []map[string]any `json:"payouts"`
	Codes        []map[string]any `json:"codes"`
	Coupons      []map[string]any `json:"coupons"`
	Badges       []map[string]any `json:"badges"`
	XPTotal      map[string]any   `json:"xp_total"`
	TaxDocuments []map[string]any `json:"tax_documents"`
	Counters     map[string]int64 `json:"counters"`
}

// Export collects every stored row attributable to the affiliate. It is
// read-only; the operator hands the JSON to the subject.
func (s *Store) Export(ctx context.Context, affiliateID int64) (*ExportBundle, error) {
	b := &ExportBundle{GeneratedAt: s.now().UTC().Format(time.RFC3339)}
	var err error
	if b.Subject, err = s.one(ctx, `SELECT id, email, name, status, postback_url,
		sponsor_id, placement_parent_id, placement_leg, created_at
		FROM affiliates WHERE id = ?`, affiliateID); err != nil {
		return nil, err
	}
	if b.Subject == nil {
		return nil, ErrUnknownAffiliate
	}
	// The portal application (pre-identity row) is keyed by email, not id.
	if b.Application, err = s.one(ctx, `SELECT id, email, name, note, status, created_at, reviewed_at
		FROM affiliate_applications WHERE email = ?`, b.Subject["email"]); err != nil {
		return nil, err
	}
	if b.Clicks, err = s.many(ctx, `SELECT id, ts, country, bot_class, utm_source, subid, placement,
		referer, landing_path, extra_json
		FROM clicks WHERE affiliate_id = ? ORDER BY ts`, affiliateID); err != nil {
		return nil, err
	}
	if b.Attributions, err = s.many(ctx, `SELECT id, click_id, campaign_id, method, created_at, expires_at
		FROM attributions WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	if b.Customers, err = s.many(ctx, `SELECT id, program_id, external_customer_id, bound_at, bound_method
		FROM customers WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	if b.Commissions, err = s.many(ctx, `SELECT id, campaign_id, amount, currency, status, matures_at, approved_at,
		paid_at, created_at, buyer_email, attribution_method, coupon_code, country, gross_minor
		FROM commissions WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	if b.Payouts, err = s.many(ctx, `SELECT id, amount, currency, method, status, provider_ref, created_at, processed_at
		FROM payouts WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	if b.Codes, err = s.many(ctx, `SELECT id, code, is_primary, source, campaign_id, created_at
		FROM affiliate_codes WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	if b.Coupons, err = s.many(ctx, `SELECT id, code, rate_type, rate_value, active, created_at
		FROM coupons WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	if b.Badges, err = s.many(ctx, `SELECT badge_code, awarded_at
		FROM affiliate_badges WHERE affiliate_id = ? ORDER BY awarded_at`, affiliateID); err != nil {
		return nil, err
	}
	if b.XPTotal, err = s.one(ctx, `SELECT xp, level, updated_at FROM xp_totals WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	}
	if b.TaxDocuments, err = s.many(ctx, `SELECT id, doc_type, status, reviewed_at, created_at
		FROM tax_documents WHERE affiliate_id = ? ORDER BY id`, affiliateID); err != nil {
		return nil, err
	}
	b.Counters = map[string]int64{
		"clicks":       int64(len(b.Clicks)),
		"attributions": int64(len(b.Attributions)),
		"customers":    int64(len(b.Customers)),
		"commissions":  int64(len(b.Commissions)),
		"payouts":      int64(len(b.Payouts)),
		"codes":        int64(len(b.Codes)),
		"coupons":      int64(len(b.Coupons)),
	}
	return b, nil
}

// JSON renders the bundle as the delivery artifact.
func (b *ExportBundle) JSON() ([]byte, error) { return json.MarshalIndent(b, "", "  ") }

// EraseReport documents what an erase did — the operator's Art. 30
// record and the subject's Art. 19 receipt.
type EraseReport struct {
	AffiliateID   int64            `json:"affiliate_id"`
	At            string           `json:"at"`
	Scrubbed      map[string]int64 `json:"scrubbed"`
	Deleted       map[string]int64 `json:"deleted"`
	PreservedNote string           `json:"preserved_note"`
}

const preservedNote = "ledger journals, commissions, payouts, daily aggregates and MLM " +
	"structure are financial records and remain booked under the pseudonymous affiliate " +
	"id; raw_events/webhook_events payloads are provider billing records (replay-dedupe " +
	"contract); all identity fields attributable to the subject were scrubbed"

// Erase executes the right-to-erasure for one affiliate: identity is
// scrubbed in place, money rows preserved with pseudonymous ids. Every
// statement is guarded so a second run reports zero rows touched (the
// one deliberate exception: the audit_log erasure record appends once
// per request — erasure history is itself an operator record).
func (s *Store) Erase(ctx context.Context, affiliateID int64) (*EraseReport, error) {
	// The OLD email must be read BEFORE rotation: the application row is
	// keyed by it, and every later statement matches on scrub-state.
	var oldEmail string
	err := s.db.QueryRowContext(ctx, `SELECT email FROM affiliates WHERE id = ?`, affiliateID).Scan(&oldEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownAffiliate
	}
	if err != nil {
		return nil, fmt.Errorf("privacy: erase lookup: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("privacy: erase begin: %w", err)
	}
	defer tx.Rollback()
	rep := &EraseReport{
		AffiliateID:   affiliateID,
		At:            s.now().UTC().Format(time.RFC3339),
		Scrubbed:      map[string]int64{},
		Deleted:       map[string]int64{},
		PreservedNote: preservedNote,
	}
	nowStr := rep.At

	// 1) Portal application (pre-identity row, keyed by the OLD email).
	if n, err := exec(tx, ctx, "affiliate_applications",
		`DELETE FROM affiliate_applications WHERE email = ?`, oldEmail); err != nil {
		return nil, err
	} else {
		rep.Deleted["affiliate_applications"] = n
	}

	// 2) Affiliate identity row: status 'removed' (the audit trigger
	// records the transition), email rotated to the id-qualified erased
	// form so UNIQUE(email) survives and re-erasure is a no-op.
	if n, err := exec(tx, ctx, "affiliates", `UPDATE affiliates SET
		email = ?, name = '', postback_url = '', postback_secret = '', status = 'removed'
		WHERE id = ? AND status != 'removed'`,
		fmt.Sprintf("erased+%d@erased.invalid", affiliateID), affiliateID); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["affiliates"] = n
	}

	// 3) Access termination: sessions, one-time grants, affiliate-scoped
	// API keys (admin keys are the operator's, not the subject's).
	for _, stmt := range []struct {
		table string
		sql   string
		args  []any
	}{
		{"affiliate_sessions", `DELETE FROM affiliate_sessions WHERE affiliate_id = ?`, []any{affiliateID}},
		{"portal_login_grants", `DELETE FROM portal_login_grants WHERE affiliate_id = ?`, []any{affiliateID}},
		{
			"api_keys", `UPDATE api_keys SET revoked_at = ? WHERE scope = ? AND revoked_at IS NULL`,
			[]any{nowStr, fmt.Sprintf("affiliate:%d", affiliateID)},
		},
	} {
		n, err := exec(tx, ctx, stmt.table, stmt.sql, stmt.args...)
		if err != nil {
			return nil, err
		}
		if stmt.table == "api_keys" {
			rep.Scrubbed[stmt.table] = n
		} else {
			rep.Deleted[stmt.table] = n
		}
	}

	// 4) Clicks: identity hashes, referer and free-text dims scrubbed in
	// place (aggregate counters reference these rows by day).
	if n, err := exec(tx, ctx, "clicks", `UPDATE clicks SET
		ip_hash = NULL, ua_hash = NULL, referer = '', extra_json = '{}'
		WHERE affiliate_id = ? AND (ip_hash IS NOT NULL OR ua_hash IS NOT NULL OR referer != '' OR extra_json != '{}')`,
		affiliateID); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["clicks"] = n
	}

	// 5) uniq_daily carries ip_hash blobs — derived counters, deletable.
	if n, err := exec(tx, ctx, "uniq_daily", `DELETE FROM uniq_daily WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["uniq_daily"] = n
	}

	// 6) Attributions: derivable analytics, not money.
	if n, err := exec(tx, ctx, "attributions", `DELETE FROM attributions WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["attributions"] = n
	}

	// 7) Customer lifetime bindings: the identity link is severed. (The
	// binding's absence stops future lifetime commissions — correct.)
	if n, err := exec(tx, ctx, "customers", `DELETE FROM customers WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["customers"] = n
	}

	// 8) Commission PII columns scrubbed in place. The money-immutable
	// triggers permit this: only amount/journal_id changes abort.
	if n, err := exec(tx, ctx, "commissions", `UPDATE commissions SET
		buyer_email = '', customer_ext_id = '', click_id = NULL, order_id = NULL, coupon_code = ''
		WHERE affiliate_id = ? AND (buyer_email != '' OR customer_ext_id != '' OR click_id IS NOT NULL OR order_id IS NOT NULL OR coupon_code != '')`,
		affiliateID); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["commissions"] = n
	}

	// Fraud verdicts: reason text can quote emails. Both subject
	// formats are matched ("1" and "affiliate:1" — the ruleset mixes them).
	if n, err := exec(tx, ctx, "fraud_verdicts", `UPDATE fraud_verdicts SET reason = '[erased]'
		WHERE subject IN (?, ?) AND reason != '[erased]'`,
		fmt.Sprint(affiliateID), fmt.Sprintf("affiliate:%d", affiliateID)); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["fraud_verdicts"] += n
	}

	// 9b) coupon_hijack verdicts carry the COUPON CODE as subject — the
	// affiliate's own identity string. These rows are invisible to the
	// id-form match above, so they are scrubbed by joining the subject's
	// codes/coupons BEFORE step 11 deletes those rows. (click_velocity
	// verdicts carry the BUYER's ip hash as subject — a different data
	// subject; out of scope for this erasure. Rules must never quote
	// another subject's identifiers in reason text.)
	if n, err := exec(tx, ctx, "fraud_verdicts", `UPDATE fraud_verdicts SET reason = '[erased]'
		WHERE reason != '[erased]'
		  AND (subject IN (SELECT code FROM affiliate_codes WHERE affiliate_id = ?)
		    OR subject IN (SELECT code FROM coupons WHERE affiliate_id = ?))`,
		affiliateID, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["fraud_verdicts"] += n
	}

	// 9c) verdicts can quote the subject's email ANYWHERE: duplicate_account
	// fires per-account over a shared alias family (its reason sits on
	// OTHER affiliates' rows), and self_referral quotes the raw buyer
	// email — any plus-alias/dot-variant of the subject's family. Match
	// the whole producible variant set.
	{
		rawEmail := strings.ToLower(strings.TrimSpace(oldEmail))
		if rawEmail != "" { // "" would produce a match-everything pattern
			patSet := map[string]struct{}{}
			for _, p := range emailResiduePatterns(oldEmail) {
				patSet[p] = struct{}{}
			}
			q := `UPDATE fraud_verdicts SET reason = '[erased]'
			WHERE reason != '[erased]' AND (`
			var args []any
			for p := range patSet {
				if len(args) > 0 {
					q += ` OR `
				}
				q += `reason LIKE ? ESCAPE '\'`
				args = append(args, p)
			}
			q += `)`
			n, err := exec(tx, ctx, "fraud_verdicts", q, args...)
			if err != nil {
				return nil, err
			}
			rep.Scrubbed["fraud_verdicts"] += n
		}
	}

	// 10) Commission holds: reason/note text scrubbed (join through the
	// affiliate's commissions; holds are enforcement history, kept).
	if n, err := exec(tx, ctx, "commission_holds", `UPDATE commission_holds SET reason = '[erased]', note = ''
		WHERE reason != '[erased]' AND commission_id IN
		(SELECT id FROM commissions WHERE affiliate_id = ?)`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Scrubbed["commission_holds"] = n
	}

	// 11) Link identity: referral codes and coupons stop resolving.
	for _, table := range []string{"affiliate_codes", "coupons"} {
		if n, err := exec(tx, ctx, table, fmt.Sprintf(`DELETE FROM %s WHERE affiliate_id = ?`, table), affiliateID); err != nil {
			return nil, err
		} else {
			rep.Deleted[table] = n
		}
	}

	// 12) Gamification state: derivable from (deleted) activity.
	for _, table := range []string{"xp_events", "xp_totals", "affiliate_badges"} {
		if n, err := exec(tx, ctx, table, fmt.Sprintf(`DELETE FROM %s WHERE affiliate_id = ?`, table), affiliateID); err != nil {
			return nil, err
		} else {
			rep.Deleted[table] = n
		}
	}

	// 12b) Payout details (PayPal/Wise email, bank details), sealed.
	if n, err := exec(tx, ctx, "affiliate_payout_details", `DELETE FROM affiliate_payout_details WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["affiliate_payout_details"] = n
	}
	if n, err := exec(tx, ctx, "payout_details_asks", `DELETE FROM payout_details_asks WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["payout_details_asks"] = n
	}
	if n, err := exec(tx, ctx, "email_log", `DELETE FROM email_log WHERE kind IN ('tips', 'recap') AND subject_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["email_log"] = n
	}

	// 13) Tax documents: reference/note can carry names; uploaded forms
	// (W-9/W-8 PDFs) carry the TIN. Files first (FK to tax_documents).
	if n, err := exec(tx, ctx, "tax_document_files", `DELETE FROM tax_document_files WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["tax_document_files"] = n
	}
	if n, err := exec(tx, ctx, "tax_documents", `DELETE FROM tax_documents WHERE affiliate_id = ?`, affiliateID); err != nil {
		return nil, err
	} else {
		rep.Deleted["tax_documents"] = n
	}

	// 14) Import undo records embed pre-import emails; surgically blank
	// this subject's entries without breaking the rest of the batch.
	nUndo, err := scrubImportUndos(ctx, tx, affiliateID)
	if err != nil {
		return nil, err
	}
	rep.Scrubbed["import_undos"] = nUndo

	// 15) The erasure itself is auditable (Art. 30). audit_log is
	// INSERT-only. The record is appended only when this call actually
	// erased something (the affiliates scrub is 1 exactly on the first
	// run) — repeat calls are true no-ops, so the audit trail shows one
	// erasure per subject, not one per operator click.
	if rep.Scrubbed["affiliates"] == 1 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audit_log (actor, table_name, row_id, action, detail)
			VALUES ('system', 'affiliates', ?, 'erasure', 'gdpr: subject data erased; money rows preserved')`,
			affiliateID); err != nil {
			return nil, fmt.Errorf("privacy: erase audit: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("privacy: erase commit: %w", err)
	}
	return rep, nil
}

// exec runs one statement and returns rows affected.
func exec(tx *sql.Tx, ctx context.Context, table, q string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("privacy: erase %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// likeEscape neutralizes LIKE wildcards in a literal and declares the \
// escape char — portable across SQLite and PostgreSQL.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// emailResiduePatterns returns the LIKE patterns (already %-wrapped and
// wildcard-escaped) for every string form of the subject's email that a
// fraud rule might have quoted in a verdict reason:
//
//   - the raw stored email (what self_referral compares against, and what
//     duplicate_account quotes when the erased account IS the family base),
//   - the alias-normalized form (what duplicate_account quotes),
//   - the plus-tagged raw form (self_referral quotes q.Email verbatim —
//     a buyer using a +tag alias of the subject's address).
//
// Gmail dot-stripping converges to the normalized form (already covered).
func emailResiduePatterns(email string) []string {
	raw := strings.ToLower(strings.TrimSpace(email))
	norm := fraud.NormalizeEmail(email)
	pats := []string{"%" + likeEscape(raw) + "%"}
	if norm != raw && norm != "" {
		pats = append(pats, "%"+likeEscape(norm)+"%")
	}
	// plus-tagged variants of the raw local part ("user+anything@…" from
	// "user@…"): the tag itself is matched as a wildcard — over-matching
	// a reason that embeds "user+…@domain" is still the subject's
	// identity string, so scrubbing it is correct either way.
	if at := strings.LastIndex(raw, "@"); at > 0 && !strings.Contains(raw[:at], "+") {
		pats = append(pats, "%"+likeEscape(raw[:at])+"+%"+likeEscape(raw[at:])+"%")
	}
	return pats
}

// scrubImportUndos rewrites undo batches in place, blanking the email of
// the erased affiliate while preserving every other entry (rollback of
// the remaining rows must keep working). JSON is walked generically so
// future fields in the undo shape survive the rewrite.
func scrubImportUndos(ctx context.Context, tx *sql.Tx, affiliateID int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT batch_id, undo_json FROM import_undos`)
	if err != nil {
		return 0, fmt.Errorf("privacy: erase import_undos: %w", err)
	}
	type pending struct {
		batchID int64
		payload []byte
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("privacy: erase import_undos scan: %w", err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			continue // malformed undo JSON is not made worse by erasure
		}
		arr, ok := doc["affiliates"].([]any)
		if !ok {
			continue
		}
		changed := false
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if idf, ok := m["affiliate_id"].(float64); ok && int64(idf) == affiliateID {
				if em, ok := m["email"].(string); ok && em != "" {
					m["email"] = ""
					changed = true
				}
				// The undo entry's code/coupon strings are the
				// affiliate's link identity (deleted in step 11) —
				// they identify the subject as much as the email.
				for _, k := range []string{"code", "coupon"} {
					if sv, ok := m[k].(string); ok && sv != "" {
						m[k] = ""
						changed = true
					}
				}
			}
		}
		if changed {
			out, err := json.Marshal(doc)
			if err != nil {
				continue
			}
			updates = append(updates, pending{id, out})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("privacy: erase import_undos rows: %w", err)
	}
	_ = rows.Close()
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE import_undos SET undo_json = ? WHERE batch_id = ?`,
			string(u.payload), u.batchID); err != nil {
			return 0, fmt.Errorf("privacy: erase import_undos update: %w", err)
		}
	}
	return int64(len(updates)), nil
}

func (s *Store) one(ctx context.Context, q string, args ...any) (map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil // no row
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rowMap(cols, vals), nil
}

func (s *Store) many(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	ptrs := make([]any, len(cols))
	vals := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, rowMap(cols, vals))
	}
	return out, rows.Err()
}

// rowMap renders driver values as JSON-friendly natives. BYTEA/blob
// columns (ip_hash, ua_hash) hex-encode so the bundle is text-safe.
func rowMap(cols []string, vals []any) map[string]any {
	m := make(map[string]any, len(cols))
	for i, c := range cols {
		switch v := vals[i].(type) {
		case []byte:
			m[c] = fmt.Sprintf("%x", v)
		default:
			m[c] = v
		}
	}
	return m
}
