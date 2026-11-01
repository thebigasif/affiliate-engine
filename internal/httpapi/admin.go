// Admin management endpoints: campaigns, commission rules, affiliate
// postback URLs. Thin CRUD with validation — the money paths stay in the
// engines.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ids"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/plan"
	"github.com/thebigasif/affiliate-engine/internal/screening"
	"github.com/thebigasif/affiliate-engine/internal/settings"
	"github.com/thebigasif/affiliate-engine/internal/usage"
)

// handleCampaigns: GET list (?limit=&after_id=) / POST create.
func (a *api) handleCampaigns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, afterID, err := pageParams(r)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		rows, err := a.db.QueryContext(ctx(r), `
			SELECT id, name, status, created_at, net_terms_days, attribution_window_days, commission_months FROM campaigns
			WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		type c struct {
			ID                    int64  `json:"id"`
			Name                  string `json:"name"`
			Status                string `json:"status"`
			CreatedAt             string `json:"created_at"`
			NetTermsDays          int    `json:"net_terms_days"`
			AttributionWindowDays int    `json:"attribution_window_days"`
			CommissionMonths      int    `json:"commission_months"` // 0 = lifetime
		}
		out := []c{}
		for rows.Next() {
			var x c
			if err := rows.Scan(&x.ID, &x.Name, &x.Status, &x.CreatedAt, &x.NetTermsDays, &x.AttributionWindowDays, &x.CommissionMonths); err != nil {
				apiError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			out = append(out, x)
		}
		if err := rows.Err(); err != nil {
			apiError(w, http.StatusInternalServerError, "query iteration failed")
			return
		}
		var next int64
		if len(out) == limit {
			next = out[len(out)-1].ID
		}
		writeJSON(w, http.StatusOK, map[string]any{"campaigns": out, "next_after_id": next})
	case http.MethodPost:
		var req struct {
			Name                  string `json:"name"`
			AttributionWindowDays int    `json:"attribution_window_days"`
			NetTermsDays          *int   `json:"net_terms_days"`
			CommissionMonths      int    `json:"commission_months"` // 0 = lifetime
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			apiError(w, http.StatusBadRequest, "name required")
			return
		}
		if utf8.RuneCountInString(strings.TrimSpace(req.Name)) > maxCampaignNameRunes {
			apiError(w, http.StatusBadRequest, "name max 120 characters")
			return
		}
		// Per-program config with sane bounds (1..365 days).
		if req.AttributionWindowDays == 0 {
			req.AttributionWindowDays = 30
		}
		// Hold (NET terms): omitted = the program-wide default_hold_days
		// (default 30); an explicit 0 = no hold.
		if req.NetTermsDays == nil {
			ps, _ := settings.Load(ctx(r), a.db)
			d := ps.DefaultHoldDays
			req.NetTermsDays = &d
		}
		if req.AttributionWindowDays < 1 || req.AttributionWindowDays > 365 ||
			*req.NetTermsDays < 0 || *req.NetTermsDays > 365 {
			apiError(w, http.StatusBadRequest, "window must be 1-365 days; net terms 0-365")
			return
		}
		if req.CommissionMonths < 0 || req.CommissionMonths > settings.MaxCommissionMonths {
			apiError(w, http.StatusBadRequest, "commission_months must be 0-120 (0 = lifetime)")
			return
		}
		var id int64
		if limit := a.deps.Plan.MaxCampaigns(); limit > 0 {
			// One statement: the count and the insert can't interleave.
			err := a.db.QueryRowContext(ctx(r),
				`INSERT INTO campaigns (name, attribution_window_days, net_terms_days, commission_months)
				 SELECT ?, ?, ?, ? WHERE (SELECT COUNT(*) FROM campaigns) < ? RETURNING id`,
				strings.TrimSpace(req.Name), req.AttributionWindowDays, *req.NetTermsDays, req.CommissionMonths, limit).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				apiError(w, http.StatusPaymentRequired, a.deps.Plan.Denied(plan.FeatMultiProgram))
				return
			}
			if err != nil {
				apiError(w, http.StatusInternalServerError, "create failed")
				return
			}
		} else if err := a.db.QueryRowContext(ctx(r),
			`INSERT INTO campaigns (name, attribution_window_days, net_terms_days, commission_months)
			 VALUES (?, ?, ?, ?) RETURNING id`,
			strings.TrimSpace(req.Name), req.AttributionWindowDays, *req.NetTermsDays, req.CommissionMonths).Scan(&id); err != nil {
			apiError(w, http.StatusInternalServerError, "create failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "attribution_window_days": req.AttributionWindowDays, "net_terms_days": *req.NetTermsDays,
			"commission_months": req.CommissionMonths,
		})
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// handleRules: GET ?campaign_id=&limit= list / POST create.
// POST body: {"campaign_id":1,"affiliate_id":null,"rate_type":"percent",
//
//	"rate_value":1000,"priority":0}
//
// Validation: percent rates are basis points (0..10000); fixed >= 0.
// The list keeps its resolution order (campaign, priority) and is bounded
// by ?limit= (default 200, max 1000).
func (a *api) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _, err := pageParams(r)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		camp := r.URL.Query().Get("campaign_id")
		q := `SELECT id, campaign_id, COALESCE(affiliate_id, 0), event_type,
			       rate_type, rate_value, priority, active, COALESCE(product_key, ''), COALESCE(currency, '')
			  FROM commission_rules`
		var rows *sql.Rows
		if camp != "" {
			campID, convErr := strconv.ParseInt(camp, 10, 64)
			if convErr != nil {
				apiError(w, http.StatusBadRequest, "campaign_id must be an integer")
				return
			}
			rows, err = a.db.QueryContext(ctx(r), q+" WHERE campaign_id = ? ORDER BY campaign_id, priority DESC, id LIMIT ?", campID, limit)
		} else {
			rows, err = a.db.QueryContext(ctx(r), q+" ORDER BY campaign_id, priority DESC, id LIMIT ?", limit)
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		type rule struct {
			ID          int64  `json:"id"`
			CampaignID  int64  `json:"campaign_id"`
			AffiliateID int64  `json:"affiliate_id"`
			EventType   string `json:"event_type"`
			RateType    string `json:"rate_type"`
			RateValue   int64  `json:"rate_value"`
			Priority    int    `json:"priority"`
			Active      bool   `json:"active"`
			ProductKey  string `json:"product_key"`
			Currency    string `json:"currency,omitempty"` // a fixed rate's currency
		}
		progCur := settings.Currency(ctx(r), a.db)
		out := []rule{}
		for rows.Next() {
			var x rule
			var aff int64
			if err := rows.Scan(&x.ID, &x.CampaignID, &aff, &x.EventType,
				&x.RateType, &x.RateValue, &x.Priority, &x.Active, &x.ProductKey, &x.Currency); err != nil {
				apiError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			x.AffiliateID = aff
			switch {
			case x.RateType != "fixed":
				x.Currency = ""
			case x.Currency == "":
				x.Currency = progCur
			}
			out = append(out, x)
		}
		if err := rows.Err(); err != nil {
			apiError(w, http.StatusInternalServerError, "query iteration failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": out})
	case http.MethodPost:
		var req struct {
			CampaignID  int64  `json:"campaign_id"`
			AffiliateID *int64 `json:"affiliate_id"`
			RateType    string `json:"rate_type"`
			RateValue   int64  `json:"rate_value"`
			Priority    int    `json:"priority"`
			ProductKey  string `json:"product_key"`
			// Currency of a fixed rate_value ("" = the program currency);
			// a fixed rule only pays sales in its currency.
			Currency string `json:"currency"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if req.CampaignID <= 0 || (req.RateType != "percent" && req.RateType != "fixed") {
			apiError(w, http.StatusBadRequest, "campaign_id + rate_type (percent|fixed) required")
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
		if req.RateValue > maxFixedRateMinor {
			apiError(w, http.StatusBadRequest, "rate_value too large")
			return
		}
		if req.Priority < -maxRulePriority || req.Priority > maxRulePriority {
			apiError(w, http.StatusBadRequest, "priority must be between -1000000 and 1000000")
			return
		}
		// Product key: optional; trimmed; bounded (a key is a join/lookup
		// token, not free text). Empty string means the product-agnostic
		// rule (pre-existing semantics).
		productKey := strings.TrimSpace(req.ProductKey)
		if len(productKey) > 128 {
			apiError(w, http.StatusBadRequest, "product_key too long (max 128)")
			return
		}
		var aff any
		if req.AffiliateID != nil {
			aff = *req.AffiliateID
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
			INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value, priority, product_key, currency)
			VALUES (?, ?, 'purchase', ?, ?, ?, ?, ?) RETURNING id`,
			req.CampaignID, aff, req.RateType, req.RateValue, req.Priority, pk, cur).Scan(&id); err != nil {
			apiError(w, http.StatusInternalServerError, "create failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// handleAffiliateUpdate: PATCH /admin/affiliates/{id}
// {"postback_url": "...", "rotate_secret": bool} — SSRF-guarded at postback
// fire time (and here: scheme+host shape validated).
//
// postback_secret: auto-generated when a postback URL is first set (and on
// explicit rotate_secret) — it signs every delivery as X-AE-Signature
// (FR-29 signed outbound webhooks). The secret is returned ONCE, in the
// response to the call that created it; empty = unsigned (backward compat).
func (a *api) handleAffiliateUpdate(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		PostbackURL  *string `json:"postback_url"`
		RotateSecret bool    `json:"rotate_secret"`
		// Status pauses ("frozen") or resumes ("active") a partner. Paused:
		// no sign-in, no new sales credited, nothing paid until resumed.
		Status *string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Status != nil {
		st := *req.Status
		if st != "active" && st != "frozen" {
			apiError(w, http.StatusBadRequest, "status must be active or frozen")
			return
		}
		// A removed (erased) partner stays removed. Pausing also revokes
		// the partner's API keys in the same transaction; resuming does
		// not restore them (the operator issues a new key).
		tx, err := a.db.BeginTx(ctx(r), nil)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "update failed")
			return
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx(r),
			`UPDATE affiliates SET status = ? WHERE id = ? AND status IN ('active','frozen')`, st, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "update failed")
			return
		}
		if n, _ := res.RowsAffected(); n != 1 {
			apiError(w, http.StatusNotFound, "no such partner (or removed)")
			return
		}
		if st == "frozen" {
			if _, err := apikeys.RevokeAffiliate(ctx(r), tx, id); err != nil {
				apiError(w, http.StatusInternalServerError, "update failed")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			apiError(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	newSecret := ""
	if req.PostbackURL != nil && *req.PostbackURL != "" && !a.deps.Plan.Allows(plan.FeatPostbacks) {
		apiError(w, http.StatusPaymentRequired, a.deps.Plan.Denied(plan.FeatPostbacks))
		return
	}
	if req.PostbackURL != nil {
		u := *req.PostbackURL
		if u != "" {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				apiError(w, http.StatusBadRequest, "postback_url must be http(s)")
				return
			}
			if len(u) > 2048 || !strings.Contains(u, "://") || strings.Contains(u, " ") {
				apiError(w, http.StatusBadRequest, "postback_url malformed")
				return
			}
		}
		if _, err := a.db.ExecContext(ctx(r),
			`UPDATE affiliates SET postback_url = ? WHERE id = ?`, u, id); err != nil {
			apiError(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	// Secret lifecycle — independent of the URL field so {"rotate_secret":true}
	// alone rotates: mint when a postback URL is first set (sign from the
	// start), replace on explicit rotate. Returned ONCE, in this response.
	if req.RotateSecret || (req.PostbackURL != nil && *req.PostbackURL != "") {
		var existing string
		if err := a.db.QueryRowContext(ctx(r),
			`SELECT postback_secret FROM affiliates WHERE id = ?`, id).Scan(&existing); err == nil && (existing == "" || req.RotateSecret) {
			if s, err := ids.NewTokenHex(24); err == nil {
				if _, err := a.db.ExecContext(ctx(r),
					`UPDATE affiliates SET postback_secret = ? WHERE id = ?`, s, id); err == nil {
					newSecret = s
				}
			}
		}
	}
	resp := map[string]any{"id": id}
	if req.Status != nil {
		resp["status"] = *req.Status
	}
	if newSecret != "" {
		resp["postback_secret"] = newSecret // shown once — relay to the affiliate over a secure channel
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsageIngest: POST /admin/usage {events:[{external_id, customer_id,
// units, unit_type, occurred_at}]}; batch ingest + optional settle trigger.
func (a *api) handleUsageIngest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Events []struct {
			ExternalID    string `json:"external_id"`
			CustomerExtID string `json:"customer_id"`
			Units         int64  `json:"units"`
			UnitType      string `json:"unit_type"`
			OccurredAt    string `json:"occurred_at"`
		} `json:"events"`
		SettleDay    int64 `json:"settle_day,omitempty"` // YYYYMMDD optional
		SettleRateBP int64 `json:"settle_rate_bp,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.Events) == 0 || len(req.Events) > 10000 {
		apiError(w, http.StatusBadRequest, "events: 1..10000")
		return
	}
	ctx := r.Context()
	ing := usage.New(a.db)
	var accepted, duplicates int
	for i, ev := range req.Events {
		at := time.Now().UTC()
		if ev.OccurredAt != "" {
			if t, err := time.Parse(time.RFC3339, ev.OccurredAt); err == nil {
				at = t
			} else {
				apiError(w, http.StatusBadRequest, fmt.Sprintf("event %d: bad occurred_at (RFC3339)", i))
				return
			}
		}
		err := ing.Ingest(ctx, usage.Event{
			Source: "api", ExternalID: ev.ExternalID, CustomerExtID: ev.CustomerExtID,
			Units: ev.Units, UnitType: ev.UnitType, OccurredAt: at,
		})
		switch err {
		case nil:
			accepted++
		case usage.ErrDuplicate:
			duplicates++
		default:
			apiError(w, http.StatusBadRequest, fmt.Sprintf("event %d: %v", i, err))
			return
		}
	}
	resp := map[string]any{"accepted": accepted, "duplicates": duplicates}
	if req.SettleDay != 0 {
		if req.SettleRateBP < 0 {
			apiError(w, http.StatusBadRequest, "settle_rate_bp >= 0 required")
			return
		}
		set := usage.NewSettler(a.db, &usagePosterAdapter{a.deps.LedgerPoster})
		if a.deps.LedgerPoster == nil {
			apiError(w, http.StatusServiceUnavailable, "ledger poster not wired")
			return
		}
		set.Screen = a.usageScreen()
		n, err := set.SettleDay(ctx, req.SettleDay, req.SettleRateBP)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "settlement failed: "+err.Error())
			return
		}
		resp["settlements"] = n
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMLMRun: POST /admin/mlm/run {"start":"RFC3339","end":"RFC3339"}
// -> closes the period (tiered split). Requires MLMEngine wired.
func (a *api) handleMLMRun(w http.ResponseWriter, r *http.Request) {
	if a.deps.MLM == nil {
		apiError(w, http.StatusServiceUnavailable, "mlm engine not wired")
		return
	}
	var req struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	start, err1 := time.Parse(time.RFC3339, req.Start)
	end, err2 := time.Parse(time.RFC3339, req.End)
	if err1 != nil || err2 != nil || !end.After(start) {
		apiError(w, http.StatusBadRequest, "start/end RFC3339 with end>start required")
		return
	}
	lines, err := a.deps.MLM.RunPeriod(r.Context(), start, end)
	if err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"period_lines": lines})
}

// handleMLMPlace: POST /admin/mlm/place
// {"affiliate_id":N,"sponsor_id":M} or {"affiliate_id":N,"parent_id":P,"leg":"L"|"R"}
func (a *api) handleMLMPlace(w http.ResponseWriter, r *http.Request) {
	if a.deps.MLM == nil {
		apiError(w, http.StatusServiceUnavailable, "mlm engine not wired")
		return
	}
	var req struct {
		AffiliateID int64  `json:"affiliate_id"`
		SponsorID   int64  `json:"sponsor_id"`
		ParentID    int64  `json:"parent_id"`
		Leg         string `json:"leg"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil || req.AffiliateID <= 0 {
		apiError(w, http.StatusBadRequest, "affiliate_id required")
		return
	}
	ctx := r.Context()
	switch {
	case req.SponsorID > 0:
		if err := a.deps.MLM.PlaceInSponsorTree(ctx, req.AffiliateID, req.SponsorID); err != nil {
			apiError(w, http.StatusConflict, err.Error())
			return
		}
	case req.ParentID > 0:
		if err := a.deps.MLM.PlaceInPlacementTree(ctx, req.AffiliateID, req.ParentID, req.Leg); err != nil {
			apiError(w, http.StatusConflict, err.Error())
			return
		}
	default:
		apiError(w, http.StatusBadRequest, "sponsor_id or parent_id+leg required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"affiliate_id": req.AffiliateID, "placed": true})
}

// usageScreen screens each usage commission in its settlement
// transaction: APIDeps.UsageScreen, or else enforced screening with the
// default ruleset (usage commissions are never left unscreened).
func (a *api) usageScreen() func(ctx context.Context, tx usage.Tx, commissionID int64) error {
	screen := a.deps.UsageScreen
	if screen == nil {
		screen = screening.New(nil, fraud.NewEngine(a.db), false).ScreenUsage
	}
	return func(ctx context.Context, tx usage.Tx, commissionID int64) error {
		ltx, ok := tx.(*ledger.Tx)
		if !ok {
			return fmt.Errorf("usage screening: unexpected transaction type %T", tx)
		}
		return screen(ctx, ltx, commissionID)
	}
}

// usagePosterAdapter bridges httpapi.Poster to usage.Poster.
type usagePosterAdapter struct{ p Poster }

func (u *usagePosterAdapter) Post(ctx context.Context, idemKey, source, desc string, eff time.Time, lines []usage.Line) (int64, error) {
	ll := make([]ledger.Line, len(lines))
	for i, l := range lines {
		ll[i] = ledger.Line{Account: l.Account, Amount: l.Amount}
	}
	return u.p.Post(ctx, idemKey, source, desc, eff, ll)
}

func (u *usagePosterAdapter) Run(ctx context.Context, fn func(ctx context.Context, tx usage.Tx) error) error {
	return u.p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error { return fn(ctx, tx) })
}

// handleAffiliateUpdateQ extracts {id} from /api/v1/admin/affiliates/{id}.
func (a *api) handleAffiliateUpdateQ(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/affiliates/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusBadRequest, "bad affiliate id")
		return
	}
	a.handleAffiliateUpdate(w, r, id)
}

func ctx(r *http.Request) context.Context { return r.Context() }

var _ = time.Now
