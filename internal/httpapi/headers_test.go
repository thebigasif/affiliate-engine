package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

// HSTS goes out on HTTPS (direct, or via a trusted proxy's
// X-Forwarded-Proto) and never on plain HTTP or a forged header.
func TestHSTS_HTTPSOnly(t *testing.T) {
	proxy := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	h := httpapi.HSTS(okHandler, proxy, true)
	cases := []struct {
		name   string
		url    string
		remote string
		proto  string
		want   bool
	}{
		{"plain http", "http://aff.test/", "203.0.113.5:1234", "", false},
		{"direct tls", "https://aff.test/", "203.0.113.5:1234", "", true},
		{"trusted proxy says https", "http://aff.test/", "10.1.2.3:1234", "https", true},
		{"trusted proxy says http", "http://aff.test/", "10.1.2.3:1234", "http", false},
		{"untrusted peer forges https", "http://aff.test/", "203.0.113.5:1234", "https", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.url, nil)
		req.RemoteAddr = c.remote
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		got := rec.Header().Get("Strict-Transport-Security")
		if (got != "") != c.want {
			t.Errorf("%s: Strict-Transport-Security=%q, want set=%v", c.name, got, c.want)
		}
		if c.want && got != "max-age=15552000" {
			t.Errorf("%s: Strict-Transport-Security=%q", c.name, got)
		}
	}

	off := httpapi.HSTS(okHandler, proxy, false)
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest("GET", "https://aff.test/", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS off still sent %q", got)
	}
}

// AE_HSTS=0, false or off turns HSTS off; anything else leaves it on.
func TestHSTSFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": true, "1": true, "on": true, "0": false, "false": false, "OFF": false} {
		t.Setenv("AE_HSTS", v)
		if got := httpapi.HSTSFromEnv(); got != want {
			t.Errorf("AE_HSTS=%q: %v, want %v", v, got, want)
		}
	}
}

// Every /api/v1 response, success or error, carries nosniff and
// Referrer-Policy, like the HTML pages.
func TestAPI_SecurityHeaders(t *testing.T) {
	srv, _, key := portalEnv(t)
	for _, c := range []struct{ path, key string }{
		{"/api/v1/admin/settings", key},   // 200
		{"/api/v1/admin/settings", ""},    // 401
		{"/api/v1/portal/brand", ""},      // public
		{"/api/v1/no-such-route", ""},     // 404 from the mux
		{"/api/v1/admin/affiliates", key}, // list
	} {
		req, _ := http.NewRequest("GET", srv.URL+c.path, nil)
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s (%d): nosniff=%q referrer=%q", c.path, resp.StatusCode,
				resp.Header.Get("X-Content-Type-Options"), resp.Header.Get("Referrer-Policy"))
		}
	}
}

// The webhook limiter counts per client IP across every route it wraps,
// answers 429 with Retry-After past the limit, and keeps other IPs apart.
func TestIPLimiter(t *testing.T) {
	limit := httpapi.NewIPLimiter(3, nil)
	a, b := limit(okHandler), limit(okHandler)
	hit := func(h http.HandlerFunc, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/webhooks/stripe", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	for i := 0; i < 3; i++ {
		h := a
		if i == 2 {
			h = b // a second route shares the per-IP budget
		}
		if rec := hit(h, "198.51.100.7:1000"); rec.Code != http.StatusOK {
			t.Fatalf("hit %d: %d", i+1, rec.Code)
		}
	}
	rec := hit(a, "198.51.100.7:2000")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := hit(a, "198.51.100.8:1000"); rec.Code != http.StatusOK {
		t.Fatalf("another IP: %d", rec.Code)
	}
	if httpapi.WebhookPerIPPerMinute < 1000 {
		t.Fatalf("webhook limit %d is low enough to hit real provider bursts", httpapi.WebhookPerIPPerMinute)
	}
}

// One IP can make the program email at most 20 different addresses an
// hour. Past that it gets the same "sent" answer, and nothing is queued.
func TestPortalLoginLink_PerIPLimit(t *testing.T) {
	srv, h, _ := portalEnvWith(t, func(d *httpapi.APIDeps) { d.MailOn = true })
	for i := 0; i < 25; i++ {
		if _, err := h.DB.Exec(`INSERT INTO affiliates (email, name) VALUES (?, 'P')`, fmt.Sprintf("p%d@x.test", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		code, out := postJSON(t, srv.URL+"/api/v1/portal/login-link", fmt.Sprintf(`{"email":"p%d@x.test"}`, i), "")
		if code != 200 || out["status"] != "sent" {
			t.Fatalf("request %d: %d %v", i+1, code, out)
		}
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE topic = ?`, partnermail.TopicLoginLink).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("queued %d login links from one IP, want 20", n)
	}
}
