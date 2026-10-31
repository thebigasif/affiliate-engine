// White-label surface (FR-33): per-program portal branding + custom domain
// + embeddable widget. Branding is cosmetic only — it can never touch money
// paths; validation is strict (accent must be #RRGGBB, logo must be https,
// domain must be a bare lowercase host) because these values flow into HTML
// and JS contexts.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/money"
	"github.com/thebigasif/affiliate-engine/internal/plan"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// Branding is the resolved white-label identity for one program. Zero
// values mean "engine default".
type Branding struct {
	CampaignID    int64  `json:"campaign_id"`
	BrandName     string `json:"brand_name"`
	AccentColor   string `json:"accent_color"`
	LogoURL       string `json:"logo_url"`
	PortalDomain  string `json:"portal_domain"`
	WidgetEnabled bool   `json:"widget_enabled"`
	// UploadedLogo is the program's uploaded logo (read-only; set by
	// PUT /admin/branding/logo). The portal brand read shows it as the logo.
	UploadedLogo string `json:"uploaded_logo,omitempty"`
}

// validAccent reports whether s is exactly #RRGGBB (hex). Strict shape —
// this string is interpolated into a CSS custom property.
func validAccent(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// validPortalDomain reports whether s is a bare DNS host: lowercase
// letters/digits/hyphens/dots, no scheme, path, port, or spaces; length
// 1..253. Must not look like an IP-literal trick (no brackets).
func validPortalDomain(s string) bool {
	if s == "" || len(s) > 253 || s != strings.ToLower(s) {
		return false
	}
	if strings.ContainsAny(s, "/\\ :?#[]@!") {
		return false
	}
	labels := strings.Split(s, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		for _, c := range l {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

// validLogoURL: https only, bounded length, no whitespace or control chars.
func validLogoURL(s string) bool {
	if s == "" {
		return true
	}
	if !strings.HasPrefix(s, "https://") || len(s) > 2048 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n\"'<>()") {
		return false
	}
	return true
}

// handleBranding: GET/PUT /api/v1/admin/branding?campaign_id=N (admin).
// PUT body: {brand_name, accent_color, logo_url, portal_domain, widget_enabled}.
// PUT is upsert: an absent row is created; present row is updated. The
// partial unique index on portal_domain rejects a domain claimed by another
// program (surfaces as 409, not 500).
func (a *api) handleBranding(w http.ResponseWriter, r *http.Request) {
	campStr := r.URL.Query().Get("campaign_id")
	campID, err := strconv.ParseInt(campStr, 10, 64)
	if err != nil || campID <= 0 {
		apiError(w, http.StatusBadRequest, "campaign_id required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		var b Branding
		var widget int
		err := a.db.QueryRowContext(r.Context(), `
			SELECT campaign_id, brand_name, accent_color, logo_url, portal_domain, widget_enabled
			FROM program_branding WHERE campaign_id = ?`, campID).
			Scan(&b.CampaignID, &b.BrandName, &b.AccentColor, &b.LogoURL, &b.PortalDomain, &widget)
		if err != nil {
			b = Branding{CampaignID: campID} // no row = defaults
		}
		b.WidgetEnabled = widget == 1
		b.UploadedLogo = uploadedLogo(r.Context(), a.db, campID)
		writeJSON(w, http.StatusOK, b)
	case http.MethodPut:
		var req Branding
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid body")
			return
		}
		req.BrandName = strings.TrimSpace(req.BrandName)
		if len(req.BrandName) > 80 {
			apiError(w, http.StatusBadRequest, "brand_name max 80 chars")
			return
		}
		if req.AccentColor != "" && !validAccent(req.AccentColor) {
			apiError(w, http.StatusBadRequest, "accent_color must be #RRGGBB")
			return
		}
		if !validLogoURL(req.LogoURL) {
			apiError(w, http.StatusBadRequest, "logo_url must be https:// and well-formed")
			return
		}
		dom := strings.ToLower(strings.TrimSpace(req.PortalDomain))
		if dom != "" && !validPortalDomain(dom) {
			apiError(w, http.StatusBadRequest, "portal_domain must be a bare lowercase host (no scheme/path)")
			return
		}
		req.PortalDomain = dom
		widget := 0
		if req.WidgetEnabled {
			widget = 1
		}
		// Campaign must exist (FK target) — reject before hitting the DB error.
		var exists int
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT 1 FROM campaigns WHERE id = ?`, campID).Scan(&exists); err != nil {
			apiError(w, http.StatusNotFound, "campaign not found")
			return
		}
		_, err := a.db.ExecContext(r.Context(), `
			INSERT INTO program_branding (campaign_id, brand_name, accent_color, logo_url, portal_domain, widget_enabled, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT (campaign_id) DO UPDATE SET
				brand_name = excluded.brand_name,
				accent_color = excluded.accent_color,
				logo_url = excluded.logo_url,
				portal_domain = excluded.portal_domain,
				widget_enabled = excluded.widget_enabled,
				updated_at = excluded.updated_at`,
			campID, req.BrandName, req.AccentColor, req.LogoURL, req.PortalDomain, widget)
		if err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "ix_branding_domain") || strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate") {
				apiError(w, http.StatusConflict, "portal_domain already claimed by another program")
				return
			}
			apiError(w, http.StatusInternalServerError, "save failed")
			return
		}
		req.CampaignID = campID
		req.UploadedLogo = uploadedLogo(r.Context(), a.db, campID)
		writeJSON(w, http.StatusOK, req)
	default:
		apiError(w, http.StatusMethodNotAllowed, "GET or PUT")
	}
}

// handlePortalBrand serves the portal's own branding: ?campaign_id= for
// multi-program installs, default program 1 otherwise. Public, cacheable
// (cosmetic data, short TTL). This is what the portal shell reads at boot
// to re-skin itself before first paint.
//
// Custom-domain mode (FR-33): when no campaign_id is supplied and the
// request's Host matches a claimed portal_domain, that program's branding
// wins — a CNAME at the edge is the only operator setup required.
func (a *api) handlePortalBrand(w http.ResponseWriter, r *http.Request) {
	campID := int64(0)
	if s := r.URL.Query().Get("campaign_id"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
			campID = v
		}
	}

	var b *Branding
	if !a.deps.Plan.Allows(plan.FeatBranding) {
		// Cloud plan without branding (e.g. after a downgrade): defaults.
		w.Header().Set("Cache-Control", "no-cache") // a renamed program shows on the next load
		a.writeBrand(w, r, &Branding{AccentColor: "#2563eb"})
		return
	}
	if campID == 0 {
		// Host-based resolution first (custom domain), else program 1.
		b = BrandForHost(r.Context(), a.db, r.Host)
		if b == nil {
			campID = 1
		}
	}
	if b == nil {
		b = brandForCampaign(r.Context(), a.db, campID)
	}
	if b.AccentColor == "" {
		b.AccentColor = "#2563eb" // engine default
	}
	w.Header().Set("Cache-Control", "no-cache") // a renamed program shows on the next load
	a.writeBrand(w, r, b)
}

// writeBrand answers the portal brand read. A demo install adds a "demo"
// block (DemoInfo) the shell uses to open straight into the dashboard;
// otherwise the payload is the Branding as-is.
func (a *api) writeBrand(w http.ResponseWriter, r *http.Request, b *Branding) {
	terms := programTerms(r.Context(), a.db, b.CampaignID)
	if b.CampaignID > 0 {
		if logo := uploadedLogo(r.Context(), a.db, b.CampaignID); logo != "" {
			c := *b
			c.LogoURL, c.UploadedLogo = logo, "" // an uploaded logo wins
			b = &c
		}
	}
	if !a.deps.Demo {
		writeJSON(w, http.StatusOK, struct {
			*Branding
			Terms *ProgramTerms `json:"terms,omitempty"`
		}{b, terms})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*Branding
		Terms *ProgramTerms `json:"terms,omitempty"`
		Demo  *DemoInfo     `json:"demo"`
	}{b, terms, a.demoInfo(r.Context())})
}

// ProgramTerms is a program's public offer, shown on the partner signup
// page: the default commission and how long, the cookie and the payout
// minimum. Partners see all of it once they join anyway.
type ProgramTerms struct {
	RateType         string `json:"rate_type"`  // percent | fixed
	RateValue        int64  `json:"rate_value"` // percent: basis points; fixed: cents
	CommissionMonths int    `json:"commission_months"`
	CookieDays       int    `json:"cookie_days"`
	MinPayoutCents   int64  `json:"min_payout_cents"`
	// Currency of a fixed RateValue and of MinPayoutCents (the program
	// currency unless the rule names its own).
	Currency string `json:"currency"`
}

// programTerms reads a program's offer; nil until it has a default
// commission rule (the same one the commission engine falls back to).
func programTerms(ctx context.Context, db *sql.DB, campID int64) *ProgramTerms {
	if campID <= 0 {
		campID = 1
	}
	var t ProgramTerms
	var rateCur sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT rate_type, rate_value, currency FROM commission_rules
		WHERE campaign_id = ? AND affiliate_id IS NULL AND active = TRUE AND product_key IS NULL
		ORDER BY priority DESC LIMIT 1`, campID).Scan(&t.RateType, &t.RateValue, &rateCur); err != nil {
		return nil
	}
	if err := db.QueryRowContext(ctx, `
		SELECT commission_months, attribution_window_days FROM campaigns WHERE id = ?`, campID).
		Scan(&t.CommissionMonths, &t.CookieDays); err != nil {
		return nil
	}
	t.Currency = settings.Currency(ctx, db)
	if t.RateType == "fixed" && rateCur.String != "" {
		t.Currency = money.Normalize(rateCur.String)
	}
	if ps, err := settings.Load(ctx, db); err == nil {
		t.MinPayoutCents = ps.MinPayout(t.Currency)
	}
	return &t
}

// brandForCampaign loads one program's branding; missing row = defaults.
func brandForCampaign(ctx context.Context, db *sql.DB, campID int64) *Branding {
	var b Branding
	var widget int
	err := db.QueryRowContext(ctx, `
		SELECT campaign_id, brand_name, accent_color, logo_url, portal_domain, widget_enabled
		FROM program_branding WHERE campaign_id = ?`, campID).
		Scan(&b.CampaignID, &b.BrandName, &b.AccentColor, &b.LogoURL, &b.PortalDomain, &widget)
	if err != nil {
		return &Branding{CampaignID: campID}
	}
	b.WidgetEnabled = widget == 1
	return &b
}

// BrandForHost resolves branding by claimed portal domain (nil when the
// host is unclaimed). Exported for the main server's index-shell injection
// (custom-domain first paint).
func BrandForHost(ctx context.Context, db *sql.DB, host string) *Branding {
	if host == "" {
		return nil
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil
	}
	var b Branding
	var widget int
	err := db.QueryRowContext(ctx, `
		SELECT campaign_id, brand_name, accent_color, logo_url, portal_domain, widget_enabled
		FROM program_branding WHERE portal_domain = ?`, host).
		Scan(&b.CampaignID, &b.BrandName, &b.AccentColor, &b.LogoURL, &b.PortalDomain, &widget)
	if err != nil {
		return nil
	}
	b.WidgetEnabled = widget == 1
	return &b
}
