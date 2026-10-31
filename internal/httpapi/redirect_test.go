package httpapi_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// noRedirect client returns the FIRST response without following.
func noRedirect(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

func newServer(t *testing.T) (*httptest.Server, *db.Handles) {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
		VALUES (1, 'asif', TRUE, 'import:rewardful')`); err != nil {
		t.Fatal(err)
	}
	// Campaign row so the per-program window join resolves (LEFT JOIN makes
	// this optional, but seeding it keeps the fixture production-shaped).
	if _, err := h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'test')`); err != nil {
		t.Fatal(err)
	}
	secret, _ := tokens.NewRandomSecret(32)
	signer, _ := tokens.New(secret)
	committer := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(committer.Close)

	cfg := httpapi.Config{
		Signer:              signer,
		AllowedDestSuffixes: []string{"shop.example.com", "example.com"},
		FallbackURL:         "https://example.com/",
	}
	mux := http.NewServeMux()
	mux.Handle("/r/", httpapi.NewRedirectHandler(h.DB, cfg, committer))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, h
}

// R1: valid click redirects to allowed dest, sets signed cookie, records click+attribution.
func TestRedirect_HappyPath(t *testing.T) {
	srv, h := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/r/1?dest=https://shop.example.com/x&utm_source=nl", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "en")
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var cookieTok string
	for _, c := range resp.Cookies() {
		if c.Name == "ae_attribution" && len(c.Value) > 20 {
			cookieTok = c.Value
		}
	}
	if cookieTok == "" {
		t.Fatal("attribution cookie not set")
	}
	// The same token rides to the destination (the merchant's site cannot
	// read the engine's host-only cookie).
	if loc := resp.Header.Get("Location"); loc != "https://shop.example.com/x?aff_token="+cookieTok {
		t.Fatalf("location=%q", loc)
	}
	// Wait for batch commit.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&n)
		var a int
		h.DB.QueryRow(`SELECT COUNT(*) FROM attributions`).Scan(&a)
		if n == 1 && a == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("click/attribution not persisted")
}

// R2: open-redirect guard — disallowed dest falls back.
func TestRedirect_OpenRedirectGuard(t *testing.T) {
	srv, _ := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/r/1?dest=https://evil.attacker.com/phish", nil)
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "https://example.com/" {
		t.Fatalf("open redirect allowed: %q", loc)
	}
	// Subdomain trick: evil-example.com must NOT match example.com.
	req2, _ := http.NewRequest("GET", srv.URL+"/r/1?dest=https://evilexample.com/", nil)
	resp2, err := noRedirect(t).Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if loc := resp2.Header.Get("Location"); loc != "https://example.com/" {
		t.Fatalf("suffix trick allowed: %q", loc)
	}
}

// R3: unknown affiliate still redirects safely, no cookie.
func TestRedirect_UnknownAffiliate(t *testing.T) {
	srv, _ := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/r/999?dest=https://shop.example.com/", nil)
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "ae_attribution" {
			t.Fatal("cookie set for unknown affiliate")
		}
	}
}

// R4: bot UA — redirects (fail-open) but excluded from stats later (bot_class stored).
func TestRedirect_BotFailOpen(t *testing.T) {
	srv, h := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/r/1?dest=https://shop.example.com/", nil)
	req.Header.Set("User-Agent", "Googlebot/2.1")
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("bot must still redirect (fail-open), got %d", resp.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var bot int
		h.DB.QueryRow(`SELECT bot_class FROM clicks`).Scan(&bot)
		if bot == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("bot click not stored with bot_class=2")
}

// R5: subdomain of allowed suffix passes.
func TestRedirect_SubdomainAllowed(t *testing.T) {
	srv, _ := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/r/1?dest=https://www.example.com/page", nil)
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://www.example.com/page?aff_token=") {
		t.Fatalf("subdomain of allowlist rejected: %q", loc)
	}
}

// R6: slug referral code resolves (imported incumbent links keep working).
func TestRedirect_SlugCode(t *testing.T) {
	srv, h := newServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/r/asif?dest=https://shop.example.com/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "en")
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var hasCookie bool
	for _, c := range resp.Cookies() {
		if c.Name == "ae_attribution" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatal("slug click: no attribution cookie")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		h.DB.QueryRow(`SELECT COUNT(*) FROM clicks WHERE affiliate_id = 1`).Scan(&n)
		if n == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("slug click not attributed to affiliate 1")
}

// R7: slug is case-insensitive; unknown slug tracks nothing.
func TestRedirect_SlugCaseAndUnknown(t *testing.T) {
	srv, h := newServer(t)
	// Uppercase slug must still resolve (stored lowercase, compared lowered).
	req, _ := http.NewRequest("GET", srv.URL+"/r/ASIF?dest=https://shop.example.com/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	req.Header.Set("Accept", "text/html")
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Unknown slug: safe redirect, no cookie, no click.
	req2, _ := http.NewRequest("GET", srv.URL+"/r/nobody?dest=https://shop.example.com/", nil)
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	resp2, err := noRedirect(t).Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("status=%d", resp2.StatusCode)
	}
	for _, c := range resp2.Cookies() {
		if c.Name == "ae_attribution" {
			t.Fatal("cookie set for unknown slug")
		}
	}
	time.Sleep(150 * time.Millisecond)
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&n)
	// First (uppercase-slug) request resolved; the unknown one must not add a click.
	if n != 1 {
		t.Fatalf("clicks=%d, want 1 (unknown slug must not track)", n)
	}
	// The click records the code it came through, lower-cased (import undo
	// uses it to refuse removing a code that is in use).
	var ref string
	h.DB.QueryRow(`SELECT ref_code FROM clicks`).Scan(&ref)
	if ref != "asif" {
		t.Fatalf("clicks.ref_code=%q want asif", ref)
	}
}

// R9: FR-2 extra_json catch-all — unclaimed redirect params persist to
// clicks.extra_json.all_params (the {ts:Source:param} postback source, FR-7).
func TestRedirect_ExtraParamsCapture(t *testing.T) {
	srv, h := newServer(t)
	req, _ := http.NewRequest("GET",
		srv.URL+"/r/1?dest=https://shop.example.com/x&utm_source=nl&subid=s1&placement=p1&fbclid=abc123&gclid=x9&custom_offer=spring", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "en")
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	var extra string
	for time.Now().Before(deadline) {
		h.DB.QueryRow(`SELECT extra_json FROM clicks LIMIT 1`).Scan(&extra)
		if extra != "" && extra != "{}" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if extra == "" || extra == "{}" {
		t.Fatalf("extra_json not populated: %q", extra)
	}
	var parsed struct {
		AllParams map[string]string `json:"all_params"`
	}
	if err := json.Unmarshal([]byte(extra), &parsed); err != nil {
		t.Fatalf("extra_json malformed: %v", err)
	}
	want := map[string]string{"fbclid": "abc123", "gclid": "x9", "custom_offer": "spring"}
	if len(parsed.AllParams) != len(want) {
		t.Fatalf("all_params=%v, want %v", parsed.AllParams, want)
	}
	for k, v := range want {
		if parsed.AllParams[k] != v {
			t.Fatalf("all_params[%q]=%q, want %q", k, parsed.AllParams[k], v)
		}
	}
	// Claimed keys never leak into the catch-all.
	for _, k := range []string{"utm_source", "subid", "placement", "dest"} {
		if _, ok := parsed.AllParams[k]; ok {
			t.Fatalf("claimed key %q leaked into all_params", k)
		}
	}
}

// R10: a tracked click hands its signed token to the destination as
// aff_token: existing params keep their order and encoding, the fragment
// stays, and a stale aff_token is replaced, never duplicated.
func TestRedirect_TokenAppendedToDest(t *testing.T) {
	srv, _ := newServer(t)
	dest := "https://shop.example.com/p/a%20b?b=2&a=x+y&aff_token=stale&&c=%2F#plans"
	req, _ := http.NewRequest("GET", srv.URL+"/r/asif?dest="+url.QueryEscape(dest), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	resp, err := noRedirect(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cookieTok string
	for _, c := range resp.Cookies() {
		if c.Name == "ae_attribution" {
			cookieTok = c.Value
		}
	}
	if cookieTok == "" {
		t.Fatal("no cookie")
	}
	want := "https://shop.example.com/p/a%20b?b=2&a=x+y&&c=%2F&aff_token=" + cookieTok + "#plans"
	if loc := resp.Header.Get("Location"); loc != want {
		t.Fatalf("location=%q\n want=%q", loc, want)
	}
	if strings.Count(resp.Header.Get("Location"), "aff_token=") != 1 {
		t.Fatal("aff_token duplicated")
	}
}

// R11: no tracked click, no token: unknown and inactive codes, and a
// destination that fails the allowlist (its fallback never carries one).
func TestRedirect_NoTokenWithoutTrackedClick(t *testing.T) {
	srv, h := newServer(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email, status) VALUES (2, 'f@t.dev', 'frozen')`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/r/nobody?dest=https://shop.example.com/x", // unknown code
		"/r/2?dest=https://shop.example.com/x",      // frozen affiliate
		"/r/asif?dest=https://evil.test/x",          // disallowed dest: fallback
	} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
		resp, err := noRedirect(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); strings.Contains(loc, "aff_token") {
			t.Fatalf("%s: token leaked into %q", path, loc)
		}
	}
}

// R12: a bare /r/{code} lands on the fallback, the merchant's own site. When
// that is on the allowlist the token rides along (t.js picks it up there);
// when it is not, it never does.
func TestRedirect_BareLinkCarriesTokenToAllowedFallback(t *testing.T) {
	srv, _ := newServer(t)
	get := func(path string) string {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
		resp, err := noRedirect(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if loc := get("/r/asif"); !strings.HasPrefix(loc, "https://example.com/?aff_token=") {
		t.Fatalf("bare link: %q, want the fallback with aff_token", loc)
	}
}
