// FR-12 tiered rates: admin CRUD for the rate_tiers ladder (volume-based
// performance tiers). Engine semantics live in commissions.resolveRule.
package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// handleTiers: GET ?campaign_id=&limit= list / POST create / DELETE ?id=.
//
// POST body: {"campaign_id":1,"product_key":"","threshold_minor":100000,
//
//	"rate_type":"percent","rate_value":2500}
//
// currency (optional) is the threshold's (and a fixed rate's) currency;
// empty = the program currency. A tier only counts, and only applies to,
// sales in its currency (no FX).
//
// product_key empty = product-agnostic tier. threshold_minor >= 0
// (0 = "newcomer tier", fires from the first sale). Percent rates are
// basis points (0..10000); fixed rates minor units >= 0. The list keeps
// its ladder order (campaign, threshold) and is bounded by ?limit=.
func (a *api) handleTiers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _, err := pageParams(r)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		camp := r.URL.Query().Get("campaign_id")
		q := `SELECT id, campaign_id, COALESCE(product_key, ''), threshold_minor,
		              rate_type, rate_value, active, COALESCE(currency, ?)
		         FROM rate_tiers`
		progCur := settings.Currency(ctx(r), a.db)
		var rows *sql.Rows
		if camp != "" {
			campID, convErr := strconv.ParseInt(camp, 10, 64)
			if convErr != nil {
				apiError(w, http.StatusBadRequest, "campaign_id must be an integer")
				return
			}
			rows, err = a.db.QueryContext(ctx(r), q+" WHERE campaign_id = ? ORDER BY campaign_id, threshold_minor, id LIMIT ?", progCur, campID, limit)
		} else {
			rows, err = a.db.QueryContext(ctx(r), q+" ORDER BY campaign_id, threshold_minor, id LIMIT ?", progCur, limit)
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		type tier struct {
			ID             int64  `json:"id"`
			CampaignID     int64  `json:"campaign_id"`
			ProductKey     string `json:"product_key"`
			ThresholdMinor int64  `json:"threshold_minor"`
			RateType       string `json:"rate_type"`
			RateValue      int64  `json:"rate_value"`
			Active         bool   `json:"active"`
			Currency       string `json:"currency"` // of threshold_minor and a fixed rate_value
		}
		out := []tier{}
		for rows.Next() {
			var x tier
			if err := rows.Scan(&x.ID, &x.CampaignID, &x.ProductKey, &x.ThresholdMinor,
				&x.RateType, &x.RateValue, &x.Active, &x.Currency); err != nil {
				apiError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			out = append(out, x)
		}
		if err := rows.Err(); err != nil {
			apiError(w, http.StatusInternalServerError, "query iteration failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tiers": out})
	case http.MethodPost:
		var req struct {
			CampaignID     int64  `json:"campaign_id"`
			ProductKey     string `json:"product_key"`
			ThresholdMinor *int64 `json:"threshold_minor"`
			RateType       string `json:"rate_type"`
			RateValue      int64  `json:"rate_value"`
			Currency       string `json:"currency"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if req.CampaignID <= 0 || (req.RateType != "percent" && req.RateType != "fixed") {
			apiError(w, http.StatusBadRequest, "campaign_id + rate_type (percent|fixed) required")
			return
		}
		if req.ThresholdMinor == nil || *req.ThresholdMinor < 0 {
			apiError(w, http.StatusBadRequest, "threshold_minor required (>= 0; 0 = newcomer tier)")
			return
		}
		if req.RateType == "percent" && (req.RateValue < 0 || req.RateValue > 10000) {
			apiError(w, http.StatusBadRequest, "percent rate is basis points 0..10000")
			return
		}
		if req.RateValue < 0 {
			apiError(w, http.StatusBadRequest, "negative rate")
			return
		}
		productKey := strings.TrimSpace(req.ProductKey)
		if len(productKey) > 128 {
			apiError(w, http.StatusBadRequest, "product_key too long (max 128)")
			return
		}
		var pk any
		if productKey != "" {
			pk = productKey
		}
		cur, err := ruleCurrency(req.Currency)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		var id int64
		if err := a.db.QueryRowContext(ctx(r), `
			INSERT INTO rate_tiers (campaign_id, product_key, threshold_minor, rate_type, rate_value, currency)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
			req.CampaignID, pk, *req.ThresholdMinor, req.RateType, req.RateValue, cur).Scan(&id); err != nil {
			apiError(w, http.StatusInternalServerError, "create failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		tierID, convErr := strconv.ParseInt(id, 10, 64)
		if convErr != nil || tierID <= 0 {
			apiError(w, http.StatusBadRequest, "id required")
			return
		}
		res, err := a.db.ExecContext(ctx(r), `DELETE FROM rate_tiers WHERE id = ?`, tierID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		n, _ := res.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET, POST or DELETE")
	}
}
