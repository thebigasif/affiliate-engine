// Package importer migrates affiliate programs from incumbent SaaS trackers
// (Rewardful, FirstPromoter, Tapfiliate, Tolt) — FR-32.
//
// Contract (BUILD-PLAN W4.25, extended by the "switch offer"):
//   - slug preservation: incumbent referral codes become affiliate_codes
//     rows so existing published links keep working on day one;
//   - referrals carrying a billing customer id become lifetime customer
//     bindings (bound_method 'import:<source>'), so renewals from customers
//     an affiliate already brought keep earning here;
//   - dry run: parse + validate + review (counts, conflicts, fraud found in
//     the history), write nothing;
//   - one batch per import (import_batches), with an undo file listing
//     exactly what that batch created and a one-command Undo that removes
//     it, refusing once money has moved against it;
//   - re-running the same import creates nothing and records no batch.
//
// Money rows are NEVER imported: commissions, balances and ledger history
// stay in the incumbent (settle them there); the engine starts each
// affiliate at zero. Commission exports are read for the review and the
// side-by-side compare only. This is deliberate: replaying another
// system's money events through our invariants would either violate them
// or force fake journals. It is also why Undo never needs reversing
// entries: an import posts nothing to the ledger, so there is nothing to
// reverse, and money earned after the import is real (Undo refuses).
package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source names the incumbent.
type Source string

const (
	Rewardful     Source = "rewardful"
	FirstPromoter Source = "firstpromoter"
	Tapfiliate    Source = "tapfiliate"
	Tolt          Source = "tolt"
)

// ParseSource maps a CLI/API name to a Source.
func ParseSource(s string) (Source, bool) {
	switch Source(strings.ToLower(strings.TrimSpace(s))) {
	case Rewardful:
		return Rewardful, true
	case FirstPromoter:
		return FirstPromoter, true
	case Tapfiliate:
		return Tapfiliate, true
	case Tolt:
		return Tolt, true
	}
	return "", false
}

// Affiliate is the normalized record every parser must produce.
type Affiliate struct {
	Email      string // required, dedup key
	Name       string
	Code       string // incumbent referral slug (preserved)
	Status     string // "active" | "frozen" (maps from incumbent states)
	Coupon     string // optional coupon code attached to the affiliate
	ExternalID string // the incumbent's affiliate id (links referral rows)
	Raw        map[string]string
}

// Dataset is everything read from one rival export.
type Dataset struct {
	Affiliates  []Affiliate
	Referrals   []Referral
	Commissions []Commission
	// Files are the parse reports of the referral/commission files
	// (their issues are merged into the migration report).
	Files []*FileInfo
}

func (d Dataset) file(kind string) *FileInfo {
	for _, f := range d.Files {
		if f != nil && f.Kind == kind {
			return f
		}
	}
	return nil
}

// Report summarizes a parse or migration.
type Report struct {
	Source      string   `json:"source"`
	Rows        int      `json:"rows"`
	Affiliates  int      `json:"affiliates"`
	Skipped     int      `json:"skipped"`      // invalid rows (no email etc.)
	DupEmails   int      `json:"dup_emails"`   // collapsed within file
	CodeClashes int      `json:"code_clashes"` // code conflicts resolved by suffixing
	Codes       int      `json:"codes"`        // referral codes created (dry-run: would be)
	DryRun      bool     `json:"dry_run"`
	BatchID     int64    `json:"batch_id,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`

	AffiliatesCreated int              `json:"affiliates_created"`
	Links             int              `json:"links"`             // codes the imported affiliates answer to (new + already held)
	Coupons           int              `json:"coupons"`           // coupons created (dry-run: would be)
	Referrals         int              `json:"referrals"`         // referral rows read
	Bindings          int              `json:"customer_bindings"` // customer bindings created (dry-run: would be)
	Commissions       int              `json:"commissions"`       // commission rows read (never imported)
	CommissionTotals  map[string]int64 `json:"commission_totals_minor,omitempty"`
	Conflicts         Issues           `json:"conflicts,omitempty"`
	Fraud             *FraudReport     `json:"fraud,omitempty"`
	NothingNew        bool             `json:"nothing_new,omitempty"`    // re-run: everything already here
	PreviousBatch     int64            `json:"previous_batch,omitempty"` // the live batch with the same input
	Fingerprint       string           `json:"fingerprint,omitempty"`
}

// affSeats is the affiliates-file header map.
var affSeats = []seatSpec{
	{"email", []string{"email", "email address", "e-mail", "affiliate email", "promoter email"}},
	{"name", []string{"name", "full name", "first name"}},
	{"code", []string{"code", "referral code", "referral_id", "ref_id", "token", "username", "handle", "slug"}},
	{"status", []string{"status", "state"}},
	{"coupon", []string{"coupon", "coupon code", "coupons"}},
	{"ext", []string{"id", "affiliate id", "promoter id", "uuid"}},
}

// Parse reads one incumbent affiliates CSV export into normalized Affiliates.
func Parse(src Source, r io.Reader) ([]Affiliate, *Report, error) {
	rep := &Report{Source: string(src)}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // incumbents are inconsistent; we map by header
	cr.TrimLeadingSpace = true
	cr.LazyQuotes = true

	header, err := cr.Read()
	if err != nil {
		return nil, rep, fmt.Errorf("importer: read header: %w", err)
	}
	col := indexHeader(header)
	if col == nil {
		return nil, rep, fmt.Errorf("importer: %s: unrecognized export header: %v", src, header)
	}

	var out []Affiliate
	seen := map[string]bool{}
	rep.Conflicts = Issues{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, rep, fmt.Errorf("importer: row %d: %w", rep.Rows+1, err)
		}
		rep.Rows++
		get := getter(col, rec)
		email := strings.ToLower(get("email"))
		if email == "" || !strings.Contains(email, "@") {
			rep.Skipped++
			rep.Conflicts.add("invalid_email", fmt.Sprintf("row %d", rep.Rows))
			continue
		}
		if seen[email] {
			rep.DupEmails++
			rep.Conflicts.add("dup_email", email)
			continue
		}
		seen[email] = true
		a := Affiliate{
			Email:      email,
			Name:       get("name"),
			Code:       normalizeSlug(get("code")),
			ExternalID: get("ext"),
			Raw:        map[string]string{},
		}
		// Status mapping per incumbent vocabulary.
		switch strings.ToLower(get("status")) {
		case "", "active", "approved", "accepted", "enabled":
			a.Status = "active"
		default: // banned/rejected/pending -> frozen (import conservative)
			a.Status = "frozen"
		}
		if c := get("coupon"); c != "" {
			a.Coupon = strings.TrimSpace(c)
		}
		out = append(out, a)
	}
	rep.Affiliates = len(out)
	return out, rep, nil
}

// indexHeader maps semantic names to column positions per incumbent export
// shape. Returns nil when the header is not recognizable.
func indexHeader(header []string) map[string]int {
	col := mapHeader(header, affSeats)
	if _, ok := col["email"]; !ok {
		return nil
	}
	return col
}

// normalizeSlug lowercases + strips chars outside [a-z0-9_-].
func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		}
	}
	out := b.String()
	if len(out) < 2 || len(out) > 32 {
		return ""
	}
	return out
}

// importMu serializes Migrate and Undo calls within the process. Concurrent
// imports hold long read snapshots across writes — on SQLite the
// interleaving deadlocks under busy_timeout (F9); one import at a time is
// the correct posture for an operator-run migration tool anyway.
var importMu sync.Mutex

// Migrate imports parsed affiliates (no referral/commission history).
func Migrate(ctx context.Context, db *sql.DB, src Source, affs []Affiliate, dryRun bool) (*Report, error) {
	return MigrateDataset(ctx, db, src, Dataset{Affiliates: affs}, dryRun)
}

// MigrateDataset imports a whole rival export inside one transaction as a
// single batch. dry-run performs every check and the review but writes
// nothing. A run that would create nothing new records no batch.
func MigrateDataset(ctx context.Context, db *sql.DB, src Source, ds Dataset, dryRun bool) (*Report, error) {
	importMu.Lock()
	defer importMu.Unlock()
	return migrateLocked(ctx, db, src, ds, dryRun)
}

type undoAff struct {
	AffiliateID int64  `json:"affiliate_id"`
	Email       string `json:"email"`
	Code        string `json:"code,omitempty"`
	Coupon      string `json:"coupon,omitempty"`
	Created     bool   `json:"created"`
}

type undoCustomer struct {
	CustomerID  int64 `json:"customer_id"` // customers.id (row id, not the billing id)
	AffiliateID int64 `json:"affiliate_id"`
}

type undoDoc struct {
	Source     string         `json:"source"`
	ImportedAt string         `json:"imported_at"`
	Affiliates []undoAff      `json:"affiliates"`
	Customers  []undoCustomer `json:"customers,omitempty"`
}

// affInfo is one affiliate the import knows about (file or database).
type affInfo struct {
	id     int64
	email  string
	status string
}

// affIndex resolves a referral/commission row's affiliate.
type affIndex struct {
	byEmail    map[string]*affInfo
	byID       map[int64]*affInfo
	byExt      map[string]int64
	byFileCode map[string]int64
	byCode     map[string]int64 // every code here, after the import
}

func (ix *affIndex) resolve(email, extID, code string) (*affInfo, bool) {
	if email != "" {
		if a, ok := ix.byEmail[email]; ok {
			return a, true
		}
	}
	if extID != "" {
		if id, ok := ix.byExt[extID]; ok {
			return ix.byID[id], true
		}
	}
	if code != "" {
		if id, ok := ix.byFileCode[code]; ok {
			return ix.byID[id], true
		}
		if id, ok := ix.byCode[code]; ok {
			if a, ok := ix.byID[id]; ok {
				return a, true
			}
		}
	}
	return nil, false
}

func migrateLocked(ctx context.Context, db *sql.DB, src Source, ds Dataset, dryRun bool) (*Report, error) {
	affs := ds.Affiliates
	rep := &Report{Source: string(src), Affiliates: len(affs), DryRun: dryRun, Conflicts: Issues{}}
	for _, f := range ds.Files {
		if f != nil {
			rep.Conflicts.merge(f.Issues)
		}
	}
	rep.Fingerprint = fingerprint(src, ds)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Existing emails/codes/coupons (conflict detection). A code maps to
	// its owner: a code the affiliate already holds is theirs, not a clash
	// (re-running an import used to mint janeslink-3, -4... on every run).
	ix := &affIndex{
		byEmail: map[string]*affInfo{}, byID: map[int64]*affInfo{}, byExt: map[string]int64{},
		byFileCode: map[string]int64{}, byCode: map[string]int64{},
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, email, status FROM affiliates`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		a := &affInfo{}
		if err := rows.Scan(&a.id, &a.email, &a.status); err != nil {
			_ = rows.Close()
			return nil, err
		}
		a.email = strings.ToLower(a.email)
		ix.byEmail[a.email], ix.byID[a.id] = a, a
	}
	_ = rows.Close()
	existingCode := ix.byCode
	rows, err = tx.QueryContext(ctx, `SELECT code, affiliate_id FROM affiliate_codes`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var owner int64
		if err := rows.Scan(&c, &owner); err != nil {
			_ = rows.Close()
			return nil, err
		}
		existingCode[c] = owner
	}
	_ = rows.Close()
	couponOwner := map[string]int64{}
	rows, err = tx.QueryContext(ctx, `SELECT code, affiliate_id FROM coupons`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var owner int64
		if err := rows.Scan(&c, &owner); err != nil {
			_ = rows.Close()
			return nil, err
		}
		couponOwner[strings.ToLower(c)] = owner
	}
	_ = rows.Close()

	undo := undoDoc{Source: string(src), ImportedAt: time.Now().UTC().Format(time.RFC3339)}
	created := 0 // rows this run creates (would create): 0 = nothing new

	for i, a := range affs {
		info, exists := ix.byEmail[a.Email]
		var affID int64
		isNew := false
		if exists {
			affID = info.id
		} else if dryRun {
			affID = -int64(i + 1) // stand-in owner: dry-run resolves clashes too
			isNew = true
		} else {
			if err := tx.QueryRowContext(ctx,
				`INSERT INTO affiliates (email, name, status) VALUES (?, ?, ?) RETURNING id`,
				a.Email, a.Name, a.Status).Scan(&affID); err != nil {
				if !isUniqueViolation(err) {
					return nil, fmt.Errorf("importer: insert affiliate %s: %w", a.Email, err)
				}
				// Concurrent import won the email. PG aborts the tx on the
				// failed statement — roll back the whole import tx and
				// re-run it; the second pass sees the winner's row via the
				// existing-email map and proceeds (idempotent semantics).
				_ = tx.Rollback()
				return migrateLocked(ctx, db, src, ds, dryRun)
			}
			isNew = true
		}
		if isNew {
			info = &affInfo{id: affID, email: a.Email, status: a.Status}
			ix.byEmail[a.Email], ix.byID[affID] = info, info
			rep.AffiliatesCreated++
			created++
		} else {
			rep.Conflicts.add("existing_email", a.Email)
		}
		if a.ExternalID != "" {
			ix.byExt[a.ExternalID] = affID
		}
		if a.Code != "" {
			ix.byFileCode[a.Code] = affID
		}

		// The undo lists only what THIS import created: a code or coupon
		// that already existed must survive a rollback.
		code, codeCreated := a.Code, false
		if code != "" {
			// Slug preservation with clash resolution: suffix -2, -3...
			base := code
			for n := 2; ; n++ {
				owner, taken := existingCode[code]
				if !taken || owner == affID {
					break
				}
				if n == 2 {
					rep.Conflicts.add("code_taken", base)
				}
				code = fmt.Sprintf("%s-%d", base, n)
				if len(code) > 32 {
					code = fmt.Sprintf("%s%d", base[:28], n)
				}
				rep.CodeClashes++
				if n > 999 {
					return nil, fmt.Errorf("importer: code clash resolution exhausted for %q", base)
				}
			}
			if _, held := existingCode[code]; !held {
				rep.Codes++
				codeCreated = true
				created++
			}
			existingCode[code] = affID
			rep.Links++
		}
		if codeCreated && !dryRun {
			// Imported code becomes primary if the affiliate had none.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
				VALUES (?, ?, NOT EXISTS (SELECT 1 FROM affiliate_codes WHERE affiliate_id = ? AND is_primary = TRUE), 'import:`+string(src)+`')`, // #nosec G202 -- src is the Source enum (compiler-checked constant set), not user input; code values are parameters
				affID, code, affID); err != nil {
				return nil, fmt.Errorf("importer: insert code %s: %w", code, err)
			}
		}

		coupon, couponCreated := strings.ToLower(strings.TrimSpace(a.Coupon)), false
		if coupon != "" {
			owner, taken := couponOwner[coupon]
			switch {
			case taken && owner != affID:
				rep.Conflicts.add("coupon_taken", coupon)
				rep.Warnings = append(rep.Warnings,
					fmt.Sprintf("coupon %s already belongs to affiliate %d: not attached to %s", coupon, owner, a.Email))
			case !taken:
				if !dryRun {
					res, err := tx.ExecContext(ctx, `
						INSERT INTO coupons (code, affiliate_id, campaign_id, rate_type)
						VALUES (?, ?, 1, 'inherit')
						ON CONFLICT(code) DO NOTHING`,
						coupon, affID)
					if err != nil {
						return nil, fmt.Errorf("importer: coupon %s: %w", a.Coupon, err)
					}
					if n, _ := res.RowsAffected(); n != 1 {
						return nil, fmt.Errorf("importer: coupon %s appeared mid-import", coupon)
					}
				}
				couponOwner[coupon] = affID
				couponCreated = true
				rep.Coupons++
				created++
			}
		}

		u := undoAff{AffiliateID: affID, Email: a.Email, Created: isNew}
		if codeCreated {
			u.Code = code
		}
		if couponCreated {
			u.Coupon = coupon
		}
		undo.Affiliates = append(undo.Affiliates, u)
		if dryRun && exists {
			// Pre-existing affiliate: the real run would attach
			// codes/coupons and leave the affiliate row untouched.
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("email %s exists: codes/coupons attach, affiliate row untouched", a.Email))
		}
	}

	// Referrals: lifetime customer bindings. Earliest referral wins when
	// a customer appears under several affiliates.
	rep.Referrals = len(ds.Referrals)
	refs := append([]Referral(nil), ds.Referrals...)
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].At.IsZero() || refs[j].At.IsZero() {
			return false
		}
		return refs[i].At.Before(refs[j].At)
	})
	bound := map[string]int64{} // customer id -> affiliate, within this file
	for _, r := range refs {
		a, ok := ix.resolve(r.AffiliateEmail, r.AffiliateExtID, r.AffiliateCode)
		if !ok {
			rep.Conflicts.add("unknown_affiliate", refLabel("referral", r.ExternalID, firstNonEmpty(r.AffiliateEmail, r.AffiliateExtID), r.AffiliateCode))
			continue
		}
		if r.CustomerID == "" {
			rep.Conflicts.add("no_customer_id", refLabel("referral", r.ExternalID, r.CustomerEmail, ""))
			continue
		}
		if rejectedStatus(r.Status) {
			continue
		}
		if prev, seen := bound[r.CustomerID]; seen {
			if prev != a.id {
				rep.Conflicts.add("customer_multi_referral", r.CustomerID)
			}
			continue
		}
		bound[r.CustomerID] = a.id
		var owner int64
		err := tx.QueryRowContext(ctx,
			`SELECT affiliate_id FROM customers WHERE program_id = 1 AND external_customer_id = ?`, r.CustomerID).Scan(&owner)
		switch {
		case err == nil:
			if owner != a.id {
				rep.Conflicts.add("customer_bound", r.CustomerID)
			}
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if !dryRun {
			var rowID int64
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method)
				VALUES (1, ?, ?, ?) RETURNING id`,
				r.CustomerID, a.id, "import:"+string(src)).Scan(&rowID); err != nil {
				return nil, fmt.Errorf("importer: bind customer %s: %w", r.CustomerID, err)
			}
			undo.Customers = append(undo.Customers, undoCustomer{CustomerID: rowID, AffiliateID: a.id})
		}
		rep.Bindings++
		created++
	}

	// Commissions: read-only. Totals per currency, unknown affiliates.
	rep.Commissions = len(ds.Commissions)
	for _, c := range ds.Commissions {
		if _, ok := ix.resolve(c.AffiliateEmail, c.AffiliateExtID, c.AffiliateCode); !ok {
			rep.Conflicts.add("unknown_affiliate", refLabel("commission", c.ExternalID, firstNonEmpty(c.AffiliateEmail, c.AffiliateExtID), c.AffiliateCode))
		}
		if voidStatus(c.Status) || !knownCurrency(c.Currency) {
			continue
		}
		if rep.CommissionTotals == nil {
			rep.CommissionTotals = map[string]int64{}
		}
		rep.CommissionTotals[c.Currency] += c.Amount
	}

	rep.Fraud = review(src, ds, ix, couponOwner)
	if len(rep.Conflicts) == 0 {
		rep.Conflicts = nil
	}

	if created == 0 {
		rep.NothingNew = true
		if err := tx.QueryRowContext(ctx, `
			SELECT batch_id FROM import_batch_state
			WHERE fingerprint = ? AND undone_at IS NULL
			ORDER BY batch_id DESC LIMIT 1`, rep.Fingerprint).Scan(&rep.PreviousBatch); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if dryRun || created == 0 {
		// Nothing written (dry run), or nothing new to write: no batch.
		return rep, nil
	}

	var batchID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO import_batches (source, dry_run, affiliates) VALUES (?, ?, ?) RETURNING id`,
		string(src), false, len(affs)).Scan(&batchID); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(undo)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO import_undos (batch_id, undo_json) VALUES (?, ?)`, batchID, string(payload)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO import_batch_state (batch_id, fingerprint) VALUES (?, ?)`, batchID, rep.Fingerprint); err != nil {
		return nil, err
	}
	rep.BatchID = batchID
	return rep, tx.Commit()
}

func refLabel(kind, id, who, code string) string {
	parts := []string{kind}
	if id != "" {
		parts = append(parts, id)
	}
	if who != "" {
		parts = append(parts, who)
	} else if code != "" {
		parts = append(parts, "code "+code)
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// rejectedStatus: a referral the rival refused never becomes a binding.
func rejectedStatus(s string) bool {
	switch s {
	case "denied", "rejected", "fraud", "fraudulent", "blocked", "void", "voided", "spam", "declined":
		return true
	}
	return false
}

// voidStatus: a commission the rival voided counts nowhere.
func voidStatus(s string) bool {
	switch s {
	case "void", "voided", "rejected", "denied", "cancelled", "canceled", "declined", "fraud", "refunded":
		return true
	}
	return false
}

// fingerprint hashes the normalized input that a run writes from. Two
// runs of the same export share it (re-run detection).
func fingerprint(src Source, ds Dataset) string {
	var lines []string
	for _, a := range ds.Affiliates {
		lines = append(lines, strings.Join([]string{"a", a.Email, a.Name, a.Code, a.Status, strings.ToLower(a.Coupon), a.ExternalID}, "\x1f"))
	}
	for _, r := range ds.Referrals {
		lines = append(lines, strings.Join([]string{"r", r.ExternalID, r.AffiliateEmail, r.AffiliateExtID, r.AffiliateCode, r.CustomerID, r.Status}, "\x1f"))
	}
	sort.Strings(lines)
	h := sha256.New()
	h.Write([]byte(src))
	for _, l := range lines {
		h.Write([]byte{'\n'})
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RollbackExport writes the undo JSON for a batch to w.
func RollbackExport(ctx context.Context, db *sql.DB, batchID int64, w io.Writer) error {
	var payload string
	err := db.QueryRowContext(ctx,
		`SELECT undo_json FROM import_undos WHERE batch_id = ?`, batchID).Scan(&payload)
	if err == sql.ErrNoRows {
		return fmt.Errorf("importer: no import batch %d", batchID)
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, payload)
	return err
}

// isUniqueViolation reports a unique-constraint failure on either engine.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "duplicate key value violates unique constraint")
}
