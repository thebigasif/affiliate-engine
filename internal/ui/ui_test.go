package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUI_IndexServed(t *testing.T) {
	rec := httptest.NewRecorder()
	IndexHandler(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"affiliate", "portal", "admin", "Σ=0", `src="/static/app.js?v=`, `src="/static/boot.js?v=`} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type=%q", ct)
	}
}

// The shell carries no inline script: every <script> has a src, so the
// CSP can forbid inline execution.
func TestUI_NoInlineScripts(t *testing.T) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for rest := html; ; {
		i := strings.Index(rest, "<script")
		if i < 0 {
			break
		}
		end := strings.Index(rest[i:], ">")
		if end < 0 {
			t.Fatal("unterminated <script tag")
		}
		tag := rest[i : i+end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("inline script in index.html: %s", tag)
		}
		closing := strings.Index(rest[i:], "</script>")
		if closing < 0 || strings.TrimSpace(rest[i+end+1:i+closing]) != "" {
			t.Fatalf("script tag with inline body: %s", tag)
		}
		rest = rest[i+closing+len("</script>"):]
	}
	if strings.Contains(html, " onclick=") || strings.Contains(html, " onload=") {
		t.Fatal("inline event handler attribute in index.html")
	}
}

func TestUI_SecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	IndexHandler(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("X-Frame-Options missing")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("X-Content-Type-Options missing")
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("Referrer-Policy missing")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	scriptSrc := ""
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src") {
			scriptSrc = d
		}
	}
	if scriptSrc != "script-src 'self'" {
		t.Fatalf("script-src=%q want 'self' only (no unsafe-inline)", scriptSrc)
	}
}

func TestUI_StaticHandler(t *testing.T) {
	h := Handler()
	// Directory root serves the embedded index.html (FileServer convention:
	// /index.html itself 301-redirects to ./).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/ code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type=%q", ct)
	}
	// The app bundle is served same-origin with a JavaScript type and
	// carries the portal's API calls.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/static/app.js code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("app.js content-type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), "api/v1/affiliate/earnings") {
		t.Fatal("app.js missing the earnings API call")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/boot.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/api/v1/portal/brand") {
		t.Fatalf("/static/boot.js code=%d", rec.Code)
	}
	// Bogus file 404s; fs semantics block traversal outside the embed tree.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/nope.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing file code=%d want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/../go.mod", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("traversal escaped the embed tree")
	}
}

// Live demo: the banner ships hidden; only a demo install's brand payload
// (boot.js -> "ae-demo" -> app.js applyDemo) reveals it and sets the key.
func TestUI_DemoBannerHiddenUntilDemo(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	html := read("index.html")
	if !strings.Contains(html, `<div id="demo-banner" role="region" aria-label="Demo notice" hidden>`) || !strings.Contains(html, "Nothing you do here is saved.") {
		t.Fatal("demo banner missing or not hidden by default")
	}
	if boot := read("boot.js"); !strings.Contains(boot, "b.demo") || !strings.Contains(boot, `"ae-demo"`) {
		t.Fatal("boot.js does not hand the demo block to app.js")
	}
	app := read("app.js")
	for _, want := range []string{"function applyDemo", `addEventListener("ae-demo"`, `$("demo-banner").hidden = false`} {
		if !strings.Contains(app, want) {
			t.Fatalf("app.js missing %q", want)
		}
	}
}

// Payout console + portal tax form are wired: the admin console carries an
// Approve button per requested payout, PayPal/Wise CSV downloads and the
// payout settings form; the portal carries the tax-form upload.
func TestUI_PayoutConsoleAndTaxFormWired(t *testing.T) {
	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{`id="po-paypal"`, `id="po-wise"`, `id="po-body"`, `id="ps-min"`, `id="ps-hold"`, `id="tf-file"`, `id="tf-go"`} {
		if !strings.Contains(string(html), id) {
			t.Errorf("index.html missing %s", id)
		}
	}
	for _, want := range []string{
		"/api/v1/admin/payouts?status=requested", "/api/v1/admin/payouts/approve", "data-pay=",
		"/api/v1/admin/payouts/export/paypal.csv", "/api/v1/admin/payouts/export/wise.csv",
		"/api/v1/admin/settings", "/api/v1/affiliate/taxdocs", "/api/v1/admin/taxdocs/file",
		"b.disabled = true",
	} {
		if !strings.Contains(string(app), want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}

// Scripts are served under a content-hash version so a CDN or browser can
// never pair a new shell with an old app.js; unversioned requests revalidate.
func TestUI_StaticVersionedAndCached(t *testing.T) {
	rec := httptest.NewRecorder()
	IndexHandler(rec, httptest.NewRequest("GET", "/", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache-control=%q", cc)
	}
	body := rec.Body.String()
	i := strings.Index(body, `src="/static/app.js?v=`)
	if i < 0 {
		t.Fatal("app.js not versioned")
	}
	url := body[i+len(`src="`):]
	url = url[:strings.Index(url, `"`)]
	h := Handler()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned %s: %d %q", url, rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/app.js", nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("unversioned: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
