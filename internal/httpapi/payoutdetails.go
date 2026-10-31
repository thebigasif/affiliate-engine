package httpapi

// Partner payout details: where each partner wants to be paid (PayPal or
// Wise email, or bank details), entered in their portal and shown to the
// program owner next to each payout. Sealed at rest (AES-256-GCM, bound
// to the affiliate), like tax forms.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/dbutil"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// PayoutDetailsCooling is how long a partner's payouts wait after they
// change payout details they had already set. Someone who stole a portal
// session cannot redirect money before the partner sees the notice. A
// first-ever setup does not wait.
const PayoutDetailsCooling = 72 * time.Hour

// errPayoutDetailsUnreadable: a stored details row does not open (a
// rotated secret, a damaged row). Export blocks the partner's payout
// instead of paying their login email.
var errPayoutDetailsUnreadable = errors.New("payout details cannot be read")

// PayoutDetails is where a partner wants their money.
type PayoutDetails struct {
	Method      string `json:"method"` // paypal | wise | bank
	PayPalEmail string `json:"paypal_email,omitempty"`
	WiseEmail   string `json:"wise_email,omitempty"`
	Bank        string `json:"bank,omitempty"` // account name, number/IBAN, bank: free text
	UpdatedAt   string `json:"updated_at,omitempty"`
	// CoolingUntil: payouts wait until then after a change (RFC3339).
	CoolingUntil string `json:"cooling_until,omitempty"`
}

// coolingUntil returns the end of the cooling period if it is still
// running at now.
func (d *PayoutDetails) coolingUntil(now time.Time) (time.Time, bool) {
	if d == nil || d.CoolingUntil == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, d.CoolingUntil)
	if err != nil || !now.Before(t) {
		return time.Time{}, false
	}
	return t, true
}

// Masked is the details with the identifier hidden, for the audit log
// and the change notice: "PayPal: j***@example.com", "Bank: **** 1234".
func (d *PayoutDetails) Masked() string {
	if d == nil {
		return ""
	}
	switch d.Method {
	case "paypal":
		return "PayPal: " + maskEmail(d.PayPalEmail)
	case "wise":
		return "Wise: " + maskEmail(d.WiseEmail)
	case "bank":
		var digits []rune
		for _, c := range d.Bank {
			if c >= '0' && c <= '9' {
				digits = append(digits, c)
			}
		}
		if len(digits) < 4 {
			return "Bank: ****"
		}
		return "Bank: **** " + string(digits[len(digits)-4:])
	}
	return ""
}

func maskEmail(e string) string {
	at := strings.LastIndexByte(e, '@')
	if at < 1 {
		return "****"
	}
	return e[:1] + "***" + e[at:]
}

// RailEmail is the email a PayPal or Wise payout file should use, or "".
func (d *PayoutDetails) RailEmail(method string) string {
	if d == nil {
		return ""
	}
	switch method {
	case "paypal":
		return d.PayPalEmail
	case "wise":
		return d.WiseEmail
	}
	return ""
}

// PayTo is the one-line "where to send it" the owner sees.
func (d *PayoutDetails) PayTo() string {
	if d == nil {
		return ""
	}
	switch d.Method {
	case "paypal":
		return "PayPal: " + d.PayPalEmail
	case "wise":
		return "Wise: " + d.WiseEmail
	case "bank":
		return "Bank: " + strings.Join(strings.Fields(d.Bank), " ")
	}
	return ""
}

// payoutKey derives the payout-details sealing key from the tax-form key
// (itself derived from the deployment secret), domain-separated.
func payoutKey(taxDocKey []byte) []byte {
	if len(taxDocKey) != 32 {
		return nil
	}
	m := hmac.New(sha256.New, taxDocKey)
	m.Write([]byte("payout-details-key-v1"))
	return m.Sum(nil)
}

func payoutAAD(affID int64) []byte { return []byte(fmt.Sprintf("payout-details:%d", affID)) }

func sealPayoutDetails(key []byte, affID int64, d PayoutDetails) ([]byte, error) {
	aead, err := taxDocAEAD(key)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, payoutAAD(affID)), nil
}

// loadPayoutDetails returns a partner's details, nil when they have none,
// and errPayoutDetailsUnreadable when a stored row does not open.
func loadPayoutDetails(ctx context.Context, db *sql.DB, key []byte, affID int64) (*PayoutDetails, error) {
	var sealed []byte
	var updated sql.NullString
	var cooling any
	err := db.QueryRowContext(ctx, `SELECT sealed, updated_at, cooling_until FROM affiliate_payout_details WHERE affiliate_id = ?`, affID).
		Scan(&sealed, &updated, &cooling)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, ok := openPayoutDetails(key, affID, sealed)
	if !ok {
		return nil, errPayoutDetailsUnreadable
	}
	d.UpdatedAt = updated.String
	if t, ok := dbutil.AsTime(cooling); ok {
		d.CoolingUntil = t.Format(time.RFC3339)
	}
	return d, nil
}

// openPayoutDetails unseals a stored row; ok=false when it does not open.
func openPayoutDetails(key []byte, affID int64, sealed []byte) (*PayoutDetails, bool) {
	aead, err := taxDocAEAD(key)
	if err != nil || len(sealed) < aead.NonceSize() {
		return nil, false
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], payoutAAD(affID))
	if err != nil {
		return nil, false
	}
	var d PayoutDetails
	if json.Unmarshal(plain, &d) != nil {
		return nil, false
	}
	return &d, true
}

// validPayoutEmail: one plain address, no display name.
func validPayoutEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && len(s) <= 254
}

// handleAffiliatePayoutDetails: GET / PUT /api/v1/affiliate/payout-details.
// PUT body: {"method":"paypal","paypal_email":"..."} (or wise_email, or
// bank). Only the chosen method's field is kept.
func (a *api) handleAffiliatePayoutDetails(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := payoutKey(a.deps.TaxDocKey)
	if key == nil {
		apiError(w, http.StatusServiceUnavailable, "payout details are not set up on this install")
		return
	}
	if r.Method == http.MethodGet {
		d, err := loadPayoutDetails(r.Context(), a.db, key, affID)
		if err != nil && !errors.Is(err, errPayoutDetailsUnreadable) {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		// Unreadable details read as "not set": the partner enters them
		// again.
		writeJSON(w, http.StatusOK, map[string]any{"payout_details": d})
		return
	}
	// Only the partner, signed in to their portal, changes where they are
	// paid. An API key (theirs or the owner's) cannot redirect payouts.
	if !idn.Portal {
		apiError(w, http.StatusForbidden, "Payout details can only be changed in the partner portal.")
		return
	}
	var req PayoutDetails
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	d := PayoutDetails{Method: req.Method}
	switch req.Method {
	case "paypal":
		d.PayPalEmail = strings.ToLower(strings.TrimSpace(req.PayPalEmail))
		if !validPayoutEmail(d.PayPalEmail) {
			apiError(w, http.StatusBadRequest, "Enter the email address of your PayPal account.")
			return
		}
	case "wise":
		d.WiseEmail = strings.ToLower(strings.TrimSpace(req.WiseEmail))
		if !validPayoutEmail(d.WiseEmail) {
			apiError(w, http.StatusBadRequest, "Enter the email address of your Wise account.")
			return
		}
	case "bank":
		d.Bank = strings.TrimSpace(req.Bank)
		if d.Bank == "" || utf8.RuneCountInString(d.Bank) > 1000 {
			apiError(w, http.StatusBadRequest, "Enter your bank details (up to 1,000 characters).")
			return
		}
	default:
		apiError(w, http.StatusBadRequest, "method must be paypal, wise or bank")
		return
	}
	sealed, err := sealPayoutDetails(key, affID, d)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	saved, err := a.savePayoutDetails(r.Context(), key, affID, d, sealed)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payout_details": saved})
}

// savePayoutDetails stores new details. When they differ from what was
// stored it also writes an audit row (masked values only), queues the
// change notice to the partner's login email, and, when details already
// existed, starts the cooling period. One transaction.
func (a *api) savePayoutDetails(ctx context.Context, key []byte, affID int64, d PayoutDetails, sealed []byte) (*PayoutDetails, error) {
	now := a.now().UTC()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var oldSealed []byte
	var oldCooling any
	err = tx.QueryRowContext(ctx, `SELECT sealed, cooling_until FROM affiliate_payout_details WHERE affiliate_id = ?`, affID).
		Scan(&oldSealed, &oldCooling)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	before, changed := "", true
	if existed {
		before = "(could not be read)"
		if old, ok := openPayoutDetails(key, affID, oldSealed); ok {
			before = old.Masked()
			changed = old.Method != d.Method || old.PayTo() != d.PayTo()
		}
	}
	// The same details saved again change nothing: a running cooling
	// period keeps its end, and no notice goes out.
	var cooling any
	if t, ok := dbutil.AsTime(oldCooling); ok && !changed {
		cooling = t.UTC().Format(time.RFC3339)
	}
	var coolingUntil time.Time
	if changed && existed {
		coolingUntil = now.Add(PayoutDetailsCooling)
		cooling = coolingUntil.Format(time.RFC3339)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO affiliate_payout_details (affiliate_id, method, sealed, updated_at, cooling_until) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (affiliate_id) DO UPDATE SET method = excluded.method, sealed = excluded.sealed,
			updated_at = excluded.updated_at, cooling_until = excluded.cooling_until`,
		affID, d.Method, sealed, now.Format(time.RFC3339), cooling); err != nil {
		return nil, err
	}
	if changed {
		detail, err := json.Marshal(map[string]string{
			"before": before, "after": d.Masked(), "cooling_until": coolingStr(coolingUntil),
		})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO audit_log (actor, table_name, row_id, action, detail)
			VALUES (?, 'affiliate_payout_details', ?, 'payout_details_change', ?)`,
			apikeys.AffiliateScope(affID), affID, string(detail)); err != nil {
			return nil, err
		}
		if a.deps.MailOn {
			if err := partnermail.EnqueuePayoutDetailsChanged(ctx, tx, partnermail.PayoutDetailsChange{
				AffiliateID: affID, Method: d.Method, Before: before, After: d.Masked(), CoolingUntil: coolingUntil,
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	d.UpdatedAt = now.Format(time.RFC3339)
	if c, ok := cooling.(string); ok {
		d.CoolingUntil = c
	}
	return &d, nil
}

func coolingStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (a *api) now() time.Time {
	if a.deps.Now != nil {
		return a.deps.Now()
	}
	return time.Now()
}

// payoutHold says why a partner's payouts wait on their payout details,
// or "" when they may be paid. coolingOnly limits it to the cooling
// period (Approve); export also refuses details that cannot be read.
func (a *api) payoutHold(ctx context.Context, affID int64, coolingOnly bool) (string, error) {
	var n int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM affiliate_payout_details WHERE affiliate_id = ?`, affID).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	d, err := loadPayoutDetails(ctx, a.db, payoutKey(a.deps.TaxDocKey), affID)
	if errors.Is(err, errPayoutDetailsUnreadable) {
		if coolingOnly {
			return "", nil
		}
		return "This partner's payout details can't be read, so PayPal and Wise files leave them out. Ask them to enter their details again in their portal.", nil
	}
	if err != nil {
		return "", err
	}
	if t, ok := d.coolingUntil(a.now()); ok {
		return "Held until " + t.UTC().Format("2 Jan 2006 15:04 UTC") + ": this partner changed their payout details. We emailed them about the change.", nil
	}
	return "", nil
}

// guardPayouts makes the runner's PayPal and Wise files pay each
// partner's own payout details, and leave a partner out while their
// details are cooling or cannot be read.
func (a *api) guardPayouts(r *payouts.Runner) {
	if r.ExportPayee != nil {
		return
	}
	r.ExportPayee = func(ctx context.Context, _, affiliateID int64, method, accountEmail string) (string, *payouts.GateBlock, error) {
		reason, err := a.payoutHold(ctx, affiliateID, false)
		if err != nil {
			return "", nil, err
		}
		if reason != "" {
			return "", &payouts.GateBlock{Kind: payouts.GatePayee, Reason: reason}, nil
		}
		d, err := loadPayoutDetails(ctx, a.db, payoutKey(a.deps.TaxDocKey), affiliateID)
		if err != nil {
			return "", nil, err
		}
		// The address they gave for this rail; their login email otherwise.
		if e := d.RailEmail(method); e != "" {
			return e, nil, nil
		}
		return accountEmail, nil, nil
	}
}

// approveHold is payoutHold for one payout the owner marks as paid. A
// payout already in a PayPal or Wise file went to the details of that
// time, so it is never held here.
func (a *api) approveHold(ctx context.Context, payoutID int64) (string, error) {
	var affID int64
	var exported bool
	err := a.db.QueryRowContext(ctx, `
		SELECT affiliate_id, CASE WHEN exported_at IS NULL THEN 0 ELSE 1 END FROM payouts WHERE id = ?`, payoutID).
		Scan(&affID, &exported)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exported) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return a.payoutHold(ctx, affID, true)
}

// payToByAffiliate loads the one-line pay-to text for several partners.
func (a *api) payToByAffiliate(ctx context.Context, ids []int64) map[int64]*PayoutDetails {
	out := map[int64]*PayoutDetails{}
	key := payoutKey(a.deps.TaxDocKey)
	if key == nil {
		return out
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if d, err := loadPayoutDetails(ctx, a.db, key, id); err == nil && d != nil {
			out[id] = d
		}
	}
	return out
}

// SeedDemoPayoutDetails gives the demo's partners sample payout details
// (PayPal, Wise or bank in turn), leaving every fourth without, so the
// dashboard shows both. Partners who already have details are skipped.
func SeedDemoPayoutDetails(ctx context.Context, db *sql.DB, taxDocKey []byte) error {
	key := payoutKey(taxDocKey)
	if key == nil {
		return errors.New("no payout-details key")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.email, COALESCE(a.name, '') FROM affiliates a
		WHERE a.status = 'active'
		  AND NOT EXISTS (SELECT 1 FROM affiliate_payout_details d WHERE d.affiliate_id = a.id)
		ORDER BY a.id`)
	if err != nil {
		return err
	}
	type aff struct {
		id          int64
		email, name string
	}
	var affs []aff
	for rows.Next() {
		var x aff
		if err := rows.Scan(&x.id, &x.email, &x.name); err != nil {
			_ = rows.Close()
			return err
		}
		affs = append(affs, x)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, x := range affs {
		var d PayoutDetails
		switch i % 4 {
		case 0:
			d = PayoutDetails{Method: "paypal", PayPalEmail: x.email}
		case 1:
			d = PayoutDetails{Method: "wise", WiseEmail: x.email}
		case 2:
			name := x.name
			if name == "" {
				name = x.email
			}
			d = PayoutDetails{Method: "bank", Bank: name + "\nIBAN DE89 3704 0044 0532 0130 00\nDemo Bank, Berlin"}
		default:
			continue // not given yet
		}
		sealed, err := sealPayoutDetails(key, x.id, d)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO affiliate_payout_details (affiliate_id, method, sealed, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (affiliate_id) DO NOTHING`,
			x.id, d.Method, sealed, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}
