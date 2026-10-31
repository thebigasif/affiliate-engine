package httpapi

// A partner's own commission rate: "my best partner gets 40%". It is an
// affiliate rule in commission_rules (product-agnostic), which the engine
// already prefers over the program rate and any volume tier. Setting a
// new rate retires the old one; clearing puts them back on the program
// rate. Commissions already earned keep the rate they were earned at.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// OwnRate is a partner's own rate; nil means the program rate.
type OwnRate struct {
	RateType  string `json:"rate_type"`          // percent | fixed
	RateValue int64  `json:"rate_value"`         // basis points, or minor units
	Currency  string `json:"currency,omitempty"` // a fixed rate's currency
}

// handlePartnerRate: PUT /api/v1/admin/affiliates/{id}/rate
// {"rate_type":"percent","rate_value":4000} sets it; {"clear":true} clears.
func (a *api) handlePartnerRate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusBadRequest, "bad affiliate id")
		return
	}
	var req struct {
		CampaignID int64  `json:"campaign_id"`
		RateType   string `json:"rate_type"`
		RateValue  int64  `json:"rate_value"`
		Currency   string `json:"currency"` // fixed only; "" = the program currency
		Clear      bool   `json:"clear"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.CampaignID == 0 {
		req.CampaignID = 1
	}
	if !req.Clear {
		switch {
		case req.RateType != "percent" && req.RateType != "fixed":
			apiError(w, http.StatusBadRequest, "rate_type must be percent or fixed")
			return
		case req.RateValue <= 0:
			apiError(w, http.StatusBadRequest, "Enter a commission above 0.")
			return
		case req.RateType == "percent" && req.RateValue > 10000:
			apiError(w, http.StatusBadRequest, "A percentage commission can't be over 100%.")
			return
		}
	}
	var exists int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM affiliates WHERE id = ?`, id).Scan(&exists); err != nil || exists == 0 {
		apiError(w, http.StatusNotFound, "no such partner")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE commission_rules SET active = FALSE
		WHERE campaign_id = ? AND affiliate_id = ? AND product_key IS NULL AND active = TRUE`, req.CampaignID, id); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	var out *OwnRate
	if !req.Clear {
		cur, err := ruleCurrency(req.Currency)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value, priority, currency)
			VALUES (?, ?, 'purchase', ?, ?, 0, ?)`, req.CampaignID, id, req.RateType, req.RateValue, cur); err != nil {
			apiError(w, http.StatusInternalServerError, "save failed")
			return
		}
		out = &OwnRate{RateType: req.RateType, RateValue: req.RateValue}
		if req.RateType == "fixed" {
			out.Currency = settings.Currency(r.Context(), a.db)
			if c, ok := cur.(string); ok {
				out.Currency = c
			}
		}
	}
	if err := tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"own_rate": out})
}

// ownRates loads each partner's own rate in program 1 (absent = none).
func (a *api) ownRates(ctx context.Context) map[int64]*OwnRate {
	out := map[int64]*OwnRate{}
	progCur := settings.Currency(ctx, a.db)
	rows, err := a.db.QueryContext(ctx, `
		SELECT affiliate_id, rate_type, rate_value,
		       CASE WHEN rate_type = 'fixed' THEN COALESCE(currency, ?) ELSE '' END
		FROM commission_rules
		WHERE campaign_id = 1 AND affiliate_id IS NOT NULL AND product_key IS NULL AND active = TRUE
		ORDER BY affiliate_id, priority DESC, id DESC`, progCur)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var x OwnRate
		if rows.Scan(&id, &x.RateType, &x.RateValue, &x.Currency) == nil {
			if _, seen := out[id]; !seen { // the one the engine uses
				x := x
				out[id] = &x
			}
		}
	}
	return out
}

// Tier is one step of the program's bonus ladder, as partners see it.
type Tier struct {
	ThresholdMinor int64  `json:"threshold_minor"` // total sales they've sent
	RateType       string `json:"rate_type"`
	RateValue      int64  `json:"rate_value"`
	Currency       string `json:"currency"` // of ThresholdMinor (and a fixed RateValue)
}

// PartnerDeal is what a partner's portal says about their rate: their own
// rate if they have one (it beats tiers), else the bonus ladder and how
// far along it they are.
type PartnerDeal struct {
	OwnRate     *OwnRate `json:"own_rate"`
	Tiers       []Tier   `json:"tiers"`
	VolumeMinor int64    `json:"volume_minor"` // their total referred sales so far, in Currency
	// Currency of the ladder and the volume: the program currency. Tiers
	// are per currency (no FX), so sales in other currencies are not in
	// VolumeMinor and do not climb this ladder.
	Currency string `json:"currency"`
}

func (a *api) partnerDeal(ctx context.Context, affID int64) PartnerDeal {
	cur := settings.Currency(ctx, a.db)
	d := PartnerDeal{OwnRate: a.ownRates(ctx)[affID], Tiers: []Tier{}, Currency: cur}
	rows, err := a.db.QueryContext(ctx, `
		SELECT threshold_minor, rate_type, rate_value FROM rate_tiers
		WHERE campaign_id = 1 AND active = TRUE AND product_key IS NULL
		  AND COALESCE(currency, ?) = ?
		ORDER BY threshold_minor`, cur, cur)
	if err == nil {
		for rows.Next() {
			t := Tier{Currency: cur}
			if rows.Scan(&t.ThresholdMinor, &t.RateType, &t.RateValue) == nil {
				d.Tiers = append(d.Tiers, t)
			}
		}
		_ = rows.Close()
	}
	// The same basis the engine measures tiers on.
	_ = a.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(gross_minor), 0) FROM commissions
		WHERE affiliate_id = ? AND campaign_id = 1 AND currency = ? AND amount > 0
		  AND status NOT IN ('cancelled','clawback')`, affID, cur).Scan(&d.VolumeMinor)
	return d
}
