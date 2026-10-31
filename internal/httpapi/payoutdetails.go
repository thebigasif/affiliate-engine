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
)

// PayoutDetails is where a partner wants their money.
type PayoutDetails struct {
	Method      string `json:"method"` // paypal | wise | bank
	PayPalEmail string `json:"paypal_email,omitempty"`
	WiseEmail   string `json:"wise_email,omitempty"`
	Bank        string `json:"bank,omitempty"` // account name, number/IBAN, bank: free text
	UpdatedAt   string `json:"updated_at,omitempty"`
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

// loadPayoutDetails returns a partner's details, nil when they have none
// (or they can't be opened: a rotated secret reads as "not set").
func loadPayoutDetails(ctx context.Context, db *sql.DB, key []byte, affID int64) (*PayoutDetails, error) {
	var sealed []byte
	var updated sql.NullString
	err := db.QueryRowContext(ctx, `SELECT sealed, updated_at FROM affiliate_payout_details WHERE affiliate_id = ?`, affID).Scan(&sealed, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	aead, err := taxDocAEAD(key)
	if err != nil || len(sealed) < aead.NonceSize() {
		return nil, nil
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], payoutAAD(affID))
	if err != nil {
		return nil, nil
	}
	var d PayoutDetails
	if json.Unmarshal(plain, &d) != nil {
		return nil, nil
	}
	d.UpdatedAt = updated.String
	return &d, nil
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
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"payout_details": d})
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
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.ExecContext(r.Context(), `
		INSERT INTO affiliate_payout_details (affiliate_id, method, sealed, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (affiliate_id) DO UPDATE SET method = excluded.method, sealed = excluded.sealed, updated_at = excluded.updated_at`,
		affID, d.Method, sealed, now); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	d.UpdatedAt = now
	writeJSON(w, http.StatusOK, map[string]any{"payout_details": d})
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
