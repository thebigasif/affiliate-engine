package httpapi_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"
)

// Per-program config pin: a campaign with attribution_window_days=7 must
// mint a cookie + signed token whose MaxAge matches 7 days — not the
// process-wide 30d default. (PRD audit: 'per-program attribution window'
// was PARTIAL — TTL hardcoded.)
func TestRedirect_PerProgramWindow(t *testing.T) {
	srv, h := newServer(t)
	// Campaign 2: 7-day window.
	if _, err := h.DB.Exec(`INSERT INTO campaigns (id, name, attribution_window_days, net_terms_days)
		VALUES (2, 'short', 7, 15)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source, campaign_id)
		VALUES (1, 'seven', FALSE, 'native', 2)`); err != nil {
		t.Fatal(err)
	}

	// Hit the per-program code.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/r/seven")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ae_attribution" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no attribution cookie")
	}
	wantMaxAge := int((7 * 24 * time.Hour).Seconds())
	if found.MaxAge != wantMaxAge {
		t.Fatalf("cookie MaxAge=%d want %d (7-day program window)", found.MaxAge, wantMaxAge)
	}
}
