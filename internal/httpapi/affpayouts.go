package httpapi

import (
	"database/sql"
	"net/http"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
)

// handleAffiliatePayouts: GET /api/v1/affiliate/payouts. The partner's own
// payouts, newest first: what was (or is about to be) sent, how, and when.
// Failed payouts are left out: their commissions went back to the balance
// and show up in the next run.
func (a *api) handleAffiliatePayouts(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, amount, currency, method, status, created_at, processed_at
		FROM payouts WHERE affiliate_id = ? AND status <> 'failed'
		ORDER BY id DESC LIMIT 100`, affID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	type payout struct {
		ID          int64  `json:"id"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
		Method      string `json:"method"`
		Status      string `json:"status"` // requested | approved | processing | paid
		CreatedAt   string `json:"created_at"`
		PaidAt      string `json:"paid_at,omitempty"`
	}
	out := []payout{}
	for rows.Next() {
		var p payout
		var created, processed sql.NullString
		if err := rows.Scan(&p.ID, &p.AmountCents, &p.Currency, &p.Method, &p.Status, &created, &processed); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		p.CreatedAt = created.String
		if p.Status == "paid" {
			p.PaidAt = processed.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payouts": out})
}
