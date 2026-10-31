package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putBranding issues the admin PUT with a JSON body.
func putBranding(t *testing.T, url, key string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, m
}

// seedCampaign ensures the campaigns row exists (setupAPI does not seed it).
func seedCampaign(t *testing.T, f *apiFixture, id int64, name string) {
	t.Helper()
	if _, err := f.db.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (?, ?)`, id, name); err != nil {
		t.Fatal(err)
	}
}

// B1: admin branding CRUD — PUT creates, GET reads back, PUT updates.
func TestBranding_AdminUpsert(t *testing.T) {
	f := setupAPI(t)
	seedCampaign(t, f, 1, "one")
	put := func(body map[string]any) (int, map[string]any) {
		return putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey, body)
	}

	code, m := put(map[string]any{
		"brand_name": "Acme Partners", "accent_color": "#0ea5e9",
		"logo_url": "https://cdn.acme.com/logo.png", "portal_domain": "partners.acme.com",
		"widget_enabled": true,
	})
	if code != http.StatusOK {
		t.Fatalf("create code=%d body=%v", code, m)
	}
	code, m = getJSON(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey)
	if code != http.StatusOK {
		t.Fatalf("read code=%d", code)
	}
	if m["brand_name"] != "Acme Partners" || m["accent_color"] != "#0ea5e9" ||
		m["portal_domain"] != "partners.acme.com" || m["widget_enabled"] != true {
		t.Fatalf("roundtrip mismatch: %v", m)
	}

	// Update: change accent + widget; name/domain persist.
	code, m = put(map[string]any{
		"brand_name": "Acme Partners", "accent_color": "#111827",
		"logo_url": "https://cdn.acme.com/logo.png", "portal_domain": "partners.acme.com",
		"widget_enabled": false,
	})
	if code != http.StatusOK {
		t.Fatalf("update code=%d body=%v", code, m)
	}
	if _, m = getJSON(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey); m["accent_color"] != "#111827" || m["widget_enabled"] != false {
		t.Fatalf("update not applied: %v", m)
	}
}

// B2: validation — bad accent, bad logo scheme, bad domain shapes.
func TestBranding_Validation(t *testing.T) {
	f := setupAPI(t)
	seedCampaign(t, f, 1, "one")
	url := f.srv.URL + "/api/v1/admin/branding?campaign_id=1"
	bad := []map[string]any{
		{"accent_color": "blue"},                  // not #RRGGBB
		{"accent_color": "#12345"},                // too short
		{"accent_color": "#12345G"},               // non-hex
		{"logo_url": "http://cdn.acme.com/x.png"}, // http not allowed
		{"logo_url": "https://a b.com"},           // space
		{"portal_domain": "https://x.com"},        // scheme
		{"portal_domain": "a..b"},                 // empty label
		{"portal_domain": "-lead.com"},            // leading hyphen
		{"portal_domain": "x.com:8080"},           // port
		{"portal_domain": "under_score.com"},      // underscore not a DNS char
		{"brand_name": strings.Repeat("x", 81)},   // > 80
	}
	for _, body := range bad {
		code, m := putBranding(t, url, f.adminKey, body)
		if code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %v, got %d (%v)", body, code, m)
		}
	}
	// Uppercase domain is normalized to lowercase, not rejected.
	code, m := putBranding(t, url, f.adminKey, map[string]any{"portal_domain": "PARTNERS.Acme.com"})
	if code != http.StatusOK || m["portal_domain"] != "partners.acme.com" {
		t.Fatalf("normalize: code=%d %v", code, m)
	}
	// Non-admin cannot write.
	code, _ = putBranding(t, url, f.aff1Key, map[string]any{"brand_name": "nope"})
	if code != http.StatusForbidden {
		t.Fatalf("affiliate write: code=%d want 403", code)
	}
	// Unknown campaign: 404.
	code, _ = putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=999", f.adminKey,
		map[string]any{"brand_name": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("unknown campaign: code=%d want 404", code)
	}
	// campaign_id missing: 400.
	code, _ = putBranding(t, f.srv.URL+"/api/v1/admin/branding", f.adminKey,
		map[string]any{"brand_name": "x"})
	if code != http.StatusBadRequest {
		t.Fatalf("no campaign_id: code=%d want 400", code)
	}
}

// B3: one domain, one program — second program claiming it gets 409.
func TestBranding_DomainConflict(t *testing.T) {
	f := setupAPI(t)
	seedCampaign(t, f, 1, "one")
	seedCampaign(t, f, 2, "two")
	code, m := putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey,
		map[string]any{"portal_domain": "af.acme.com"})
	if code != http.StatusOK {
		t.Fatalf("first claim code=%d %v", code, m)
	}
	code, m = putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=2", f.adminKey,
		map[string]any{"portal_domain": "af.acme.com"})
	if code != http.StatusConflict {
		t.Fatalf("second claim code=%d want 409 (%v)", code, m)
	}
	// Same program re-claiming its own domain is fine (upsert).
	code, _ = putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey,
		map[string]any{"portal_domain": "af.acme.com", "brand_name": "again"})
	if code != http.StatusOK {
		t.Fatalf("self re-claim code=%d", code)
	}
	// Releasing (empty domain) then claiming by the other program works.
	code, _ = putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey,
		map[string]any{"portal_domain": ""})
	if code != http.StatusOK {
		t.Fatalf("release code=%d", code)
	}
	code, _ = putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=2", f.adminKey,
		map[string]any{"portal_domain": "af.acme.com"})
	if code != http.StatusOK {
		t.Fatalf("claim after release code=%d", code)
	}
}

// B4: portal brand read — unbranded program gets defaults; branded program
// round-trips; host resolution maps a claimed domain to its program.
func TestBranding_PortalBrandAndHost(t *testing.T) {
	f := setupAPI(t)
	seedCampaign(t, f, 1, "one")
	// Default (no row): engine defaults, widget off.
	code, m := getJSON(t, f.srv.URL+"/api/v1/portal/brand", "")
	if code != http.StatusOK {
		t.Fatalf("default brand code=%d", code)
	}
	if m["accent_color"] != "#2563eb" || m["widget_enabled"] != false {
		t.Fatalf("defaults wrong: %v", m)
	}

	// Brand program 1 with a custom domain.
	code, _ = putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey,
		map[string]any{"brand_name": "Acme", "accent_color": "#0ea5e9", "portal_domain": "partners.acme.com"})
	if code != http.StatusOK {
		t.Fatalf("brand put code=%d", code)
	}

	// Explicit campaign_id path round-trips.
	if _, m = getJSON(t, f.srv.URL+"/api/v1/portal/brand?campaign_id=1", ""); m["brand_name"] != "Acme" || m["portal_domain"] != "partners.acme.com" {
		t.Fatalf("campaign brand: %v", m)
	}

	// Host resolution: a claimed domain resolves to its program's branding;
	// an unclaimed host resolves nil (falls back to program 1).
	ctx := context.Background()
	b := BrandForHost(ctx, f.db.DB, "partners.acme.com")
	if b == nil || b.CampaignID != 1 || b.BrandName != "Acme" {
		t.Fatalf("host-resolved branding: %+v", b)
	}
	if got := BrandForHost(ctx, f.db.DB, "unclaimed.example.com"); got != nil {
		t.Fatalf("unclaimed host must resolve nil, got %+v", got)
	}
	// Host with port is normalized before lookup.
	if got := BrandForHost(ctx, f.db.DB, "partners.acme.com:8443"); got == nil {
		t.Fatal("host:port not normalized")
	}
}

// B5: widget.js — served, correct content type, embeddable (no
// X-Frame-Options), fixed origin injection, no innerHTML (XSS surface).
func TestBranding_WidgetServed(t *testing.T) {
	ts := httptest.NewServer(NewWidgetHandler(WidgetConfig{BaseURL: "https://af.acme.com"}))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("widget code=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("content type=%q", ct)
	}
	if xo := resp.Header.Get("X-Frame-Options"); xo != "" {
		t.Fatalf("widget must be embeddable, got X-Frame-Options=%q", xo)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if !strings.Contains(body, `"https://af.acme.com"`) {
		t.Fatal("origin not injected")
	}
	if !strings.Contains(body, "__aeWidget") || !strings.Contains(body, "textContent") {
		t.Fatal("launcher script shape wrong")
	}
	if strings.Contains(body, "innerHTML") {
		t.Fatal("innerHTML in widget script")
	}
}

// B6: widget origin hardening — a misconfigured BaseURL (javascript:/data:
// scheme, or a relative string) must never be embedded into the launcher;
// the handler falls back to the request's own origin instead.
func TestBranding_WidgetOriginRejectsNonHTTP(t *testing.T) {
	for _, bad := range []string{
		"javascript:alert(1)",
		"javascript://x/%0aalert(1)",
		"data:text/html,hi",
		"not a url",
		"ftp://files.example.com",
		"//protocol-relative.example.com",
	} {
		ts := httptest.NewServer(NewWidgetHandler(WidgetConfig{BaseURL: bad}))
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatalf("base=%q: %v", bad, err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		ts.Close()
		body := buf.String()
		if strings.Contains(body, bad) {
			t.Fatalf("base=%q leaked into the widget script", bad)
		}
		// Fallback origin = the test server's own origin (host includes port).
		if !strings.Contains(body, `"http://127.0.0.1:`) {
			t.Fatalf("base=%q: no safe fallback origin embedded: %s", bad, body[:min(200, len(body))])
		}
	}
}
