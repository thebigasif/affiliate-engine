package httpapi_test

// /t.js: the served response (headers, no cookies, static body) and the
// script's behaviour in a fake browser (testdata/tracker_harness.js, run
// with node; skipped when node is not installed).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

func TestTracker_Served(t *testing.T) {
	h := httpapi.NewTrackerHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t.js", nil)
	req.Host = "evil\"host.test" // must not be reflected anywhere
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("content-type=%q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("cache-control=%q", cc)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff missing")
	}
	if len(resp.Cookies()) != 0 || resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("t.js must not set cookies")
	}
	body := rec.Body.String()
	if strings.Contains(body, "evil") || !strings.Contains(body, "at_token") {
		t.Fatalf("unexpected body (host reflected or wrong script)")
	}

	// Revalidation: the ETag answers 304 with no body.
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/t.js", nil)
	req2.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Fatalf("revalidate: %d len=%d", rec2.Code, rec2.Body.Len())
	}

	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/t.js", nil))
	if rec3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", rec3.Code)
	}
}

// --- behaviour in a fake browser ---

type trackerScenario struct {
	Script          string            `json:"script"`
	Href            string            `json:"href"`
	ScriptSrc       string            `json:"scriptSrc"`
	DataParam       *string           `json:"dataParam,omitempty"`
	Cookie          string            `json:"cookie,omitempty"`
	Session         map[string]string `json:"session,omitempty"`
	SessionThrows   bool              `json:"sessionThrows,omitempty"`
	CookieThrows    bool              `json:"cookieThrows,omitempty"`
	NoCurrentScript bool              `json:"noCurrentScript,omitempty"`
	Framed          bool              `json:"framed,omitempty"`
	RunTwice        bool              `json:"runTwice,omitempty"`
	ReadyState      string            `json:"readyState,omitempty"`
	Links           []string          `json:"links,omitempty"`
	Tags            []trackerTag      `json:"tags,omitempty"`
	AddLater        *struct {
		Links []string     `json:"links,omitempty"`
		Tags  []trackerTag `json:"tags,omitempty"`
	} `json:"addLater,omitempty"`
}

type trackerTag struct {
	Tag   string            `json:"tag"`
	Attrs map[string]string `json:"attrs"`
}

type trackerResult struct {
	Replaced     *string           `json:"replaced"`
	ReplaceState *string           `json:"replaceState"`
	State        map[string]any    `json:"state"`
	CookieWrites []string          `json:"cookieWrites"`
	Errors       []string          `json:"errors"`
	Jar          map[string]string `json:"jar"`
	Session      map[string]string `json:"session"`
	Links        []string          `json:"links"`
	Tags         []map[string]string
	Observers    int `json:"observers"`
	API          *struct {
		Token             string `json:"token"`
		ClientReferenceID string `json:"clientReferenceId"`
	} `json:"api"`
}

const engineSrc = "https://acme.affiliatetracking.co/t.js"

func runTracker(t *testing.T, sc trackerScenario) trackerResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: t.js behaviour tests skipped")
	}
	if sc.Script == "" {
		sc.Script, _ = filepath.Abs("t.js")
	}
	if sc.ScriptSrc == "" {
		sc.ScriptSrc = engineSrc
	}
	b, _ := json.Marshal(sc)
	cmd := exec.Command(node, filepath.Join("testdata", "tracker_harness.js"))
	cmd.Env = append(os.Environ(), "SCENARIO="+string(b))
	outb, err := cmd.Output()
	if err != nil {
		t.Fatalf("harness: %v (%s)", err, outb)
	}
	var res trackerResult
	if err := json.Unmarshal(outb, &res); err != nil {
		t.Fatalf("harness output: %v: %s", err, outb)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("t.js threw: %v", res.Errors)
	}
	return res
}

func trackerToken(t *testing.T, ttl time.Duration) string {
	t.Helper()
	secret, _ := tokens.NewRandomSecret(32)
	s, _ := tokens.New(secret)
	tok, err := s.Sign("clk_tracker_test_0001", 7, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func strp(s string) *string { return &s }

// The token from /r/ lands in a first-party cookie that lives as long as
// the token, and leaves the visible URL (other params, hash and SPA
// history state intact).
func TestTracker_StoresTokenAndCleansURL(t *testing.T) {
	tok := trackerToken(t, 2*time.Hour)
	res := runTracker(t, trackerScenario{
		Href: "https://customer.com/pricing?a=1&aff_token=" + tok + "&b=x%20y#top",
	})
	if len(res.CookieWrites) != 1 {
		t.Fatalf("cookie writes=%v", res.CookieWrites)
	}
	c := res.CookieWrites[0]
	if !strings.HasPrefix(c, "at_token="+tok+";") || !strings.Contains(c, "path=/") ||
		!strings.Contains(c, "SameSite=Lax") || !strings.Contains(c, "Secure") {
		t.Fatalf("cookie=%q", c)
	}
	i := strings.Index(c, "max-age=")
	maxAge, _ := strconv.Atoi(strings.SplitN(c[i+8:], ";", 2)[0])
	if maxAge < 7100 || maxAge > 7200 {
		t.Fatalf("max-age=%d, want the token's remaining life (~7200)", maxAge)
	}
	if res.ReplaceState == nil || *res.ReplaceState != "/pricing?a=1&b=x%20y#top" {
		t.Fatalf("replaceState=%v", res.ReplaceState)
	}
	if res.State["spa"] != float64(1) {
		t.Fatalf("history state not preserved: %v", res.State)
	}
	if res.Replaced != nil {
		t.Fatalf("navigated with a token present: %s", *res.Replaced)
	}
	want, _ := tokens.ClientReferenceID(tok)
	if res.API == nil || res.API.Token != tok || res.API.ClientReferenceID != want {
		t.Fatalf("api=%+v, want token and client_reference_id %q (same encoding as Go)", res.API, want)
	}
}

func TestTracker_TokenEdgeCases(t *testing.T) {
	tok := trackerToken(t, 2*time.Hour)

	// Plain http: no Secure flag (the browser would drop the cookie).
	res := runTracker(t, trackerScenario{Href: "http://customer.test/?aff_token=" + tok})
	if len(res.CookieWrites) != 1 || strings.Contains(res.CookieWrites[0], "Secure") {
		t.Fatalf("http cookie=%v", res.CookieWrites)
	}

	// Expired token: not stored, still removed from the URL.
	old := trackerToken(t, -time.Hour)
	res = runTracker(t, trackerScenario{Href: "https://customer.com/?aff_token=" + old})
	if len(res.CookieWrites) != 0 || res.ReplaceState == nil || *res.ReplaceState != "/" {
		t.Fatalf("expired: writes=%v replaceState=%v", res.CookieWrites, res.ReplaceState)
	}

	// Garbage (or injection attempts) never reach document.cookie, and an
	// aff_token param, even a bad one, means no rival bounce.
	res = runTracker(t, trackerScenario{Href: "https://customer.com/?via=john&aff_token=x%3B%20domain%3D.com"})
	if len(res.CookieWrites) != 0 || res.Replaced != nil {
		t.Fatalf("garbage: writes=%v replaced=%v", res.CookieWrites, res.Replaced)
	}

	// Cookies blocked: the page that received the token still decorates
	// its Stripe links.
	res = runTracker(t, trackerScenario{
		Href:         "https://customer.com/?aff_token=" + tok,
		CookieThrows: true,
		Links:        []string{"https://buy.stripe.com/abc"},
	})
	ref, _ := tokens.ClientReferenceID(tok)
	if res.Links[0] != "https://buy.stripe.com/abc?client_reference_id="+ref {
		t.Fatalf("cookie-less decoration: %v", res.Links)
	}
}

// Rival links (?via=code) go through the engine's own /r/ with the param
// dropped from the way back, once per session.
func TestTracker_RivalParamBounce(t *testing.T) {
	res := runTracker(t, trackerScenario{Href: "https://customer.com/p?via=John&x=1#h"})
	want := "https://acme.affiliatetracking.co/r/John?dest=" + url.QueryEscape("https://customer.com/p?x=1#h")
	want = strings.ReplaceAll(want, "+", "%20")
	if res.Replaced == nil || *res.Replaced != want {
		t.Fatalf("replaced=%v\n want %s", res.Replaced, want)
	}
	if res.Session["at_bounced:via=john"] != "1" {
		t.Fatalf("loop guard not recorded: %v", res.Session)
	}

	// Same code again this session: no second bounce.
	res = runTracker(t, trackerScenario{
		Href:    "https://customer.com/p?via=John",
		Session: map[string]string{"at_bounced:via=john": "1"},
	})
	if res.Replaced != nil {
		t.Fatalf("bounced twice: %s", *res.Replaced)
	}

	// No sessionStorage: no loop guard, so no bounce.
	res = runTracker(t, trackerScenario{Href: "https://customer.com/p?via=john", SessionThrows: true})
	if res.Replaced != nil {
		t.Fatalf("bounced without a loop guard: %s", *res.Replaced)
	}

	// Framed pages never navigate.
	res = runTracker(t, trackerScenario{Href: "https://customer.com/p?via=john", Framed: true})
	if res.Replaced != nil {
		t.Fatalf("framed page navigated: %s", *res.Replaced)
	}

	// Codes are plain slugs: anything else is ignored.
	for _, bad := range []string{"..%2Fapi", "a%20b", "x%3Fy", "", strings.Repeat("a", 65)} {
		res = runTracker(t, trackerScenario{Href: "https://customer.com/p?via=" + bad})
		if res.Replaced != nil {
			t.Fatalf("via=%q navigated to %s", bad, *res.Replaced)
		}
	}
}

func TestTracker_DataParamAndOrigin(t *testing.T) {
	// data-param replaces the default; "via" then means nothing.
	res := runTracker(t, trackerScenario{
		Href:      "https://customer.com/?ref=abc",
		DataParam: strp("fpr, ref"),
		ScriptSrc: "https://Track.Example.com:8443/t.js?v=2",
	})
	if res.Replaced == nil || !strings.HasPrefix(*res.Replaced, "https://track.example.com:8443/r/abc?dest=") {
		t.Fatalf("replaced=%v", res.Replaced)
	}
	res = runTracker(t, trackerScenario{Href: "https://customer.com/?via=abc", DataParam: strp("fpr,ref")})
	if res.Replaced != nil {
		t.Fatalf("via bounced with data-param=fpr,ref: %s", *res.Replaced)
	}

	// Navigation only ever targets the script's own origin (userinfo in
	// the src cannot redirect it).
	res = runTracker(t, trackerScenario{
		Href:      "https://customer.com/?via=abc",
		ScriptSrc: "https://evil.test@acme.affiliatetracking.co/t.js",
	})
	if res.Replaced == nil || !strings.HasPrefix(*res.Replaced, "https://acme.affiliatetracking.co/r/abc?") {
		t.Fatalf("replaced=%v", res.Replaced)
	}

	// Without document.currentScript the engine origin is unknown (a
	// guess by src could pick another site's /t.js): no bounce.
	res = runTracker(t, trackerScenario{Href: "https://customer.com/?via=abc", NoCurrentScript: true})
	if res.Replaced != nil {
		t.Fatalf("navigated without knowing its own origin: %s", *res.Replaced)
	}
}

// With a token, Payment Links, the Pricing Table and the Buy Button get it
// as client_reference_id; the merchant's own values are never
// overwritten; elements added later are decorated too.
func TestTracker_StripeDecoration(t *testing.T) {
	tok := trackerToken(t, 2*time.Hour)
	ref, _ := tokens.ClientReferenceID(tok)
	res := runTracker(t, trackerScenario{
		Href:       "https://customer.com/pricing",
		Cookie:     "other=1; at_token=" + tok,
		ReadyState: "loading",
		Links: []string{
			"https://buy.stripe.com/abc",
			"https://buy.stripe.com/x?client_reference_id=mine",
			"https://buy.stripe.com/y?prefilled_email=a%40b.c",
			"https://example.com/z",
		},
		Tags: []trackerTag{
			{Tag: "stripe-pricing-table", Attrs: map[string]string{"pricing-table-id": "prctbl_1"}},
			{Tag: "stripe-buy-button", Attrs: map[string]string{"client-reference-id": "mine"}},
		},
		AddLater: &struct {
			Links []string     `json:"links,omitempty"`
			Tags  []trackerTag `json:"tags,omitempty"`
		}{
			Links: []string{"https://buy.stripe.com/later"},
			Tags:  []trackerTag{{Tag: "stripe-buy-button", Attrs: map[string]string{}}},
		},
	})
	wantLinks := []string{
		"https://buy.stripe.com/abc?client_reference_id=" + ref,
		"https://buy.stripe.com/x?client_reference_id=mine",
		"https://buy.stripe.com/y?prefilled_email=a%40b.c&client_reference_id=" + ref,
		"https://example.com/z",
		"https://buy.stripe.com/later?client_reference_id=" + ref,
	}
	for i, w := range wantLinks {
		if res.Links[i] != w {
			t.Fatalf("link %d = %q, want %q", i, res.Links[i], w)
		}
	}
	if res.Tags[0]["client-reference-id"] != ref || res.Tags[1]["client-reference-id"] != "mine" ||
		res.Tags[2]["client-reference-id"] != ref {
		t.Fatalf("tags=%v", res.Tags)
	}
	if res.Observers != 1 {
		t.Fatalf("observers=%d, want 1", res.Observers)
	}
	if back, ok := tokens.FromClientReferenceID(ref); !ok || back != tok {
		t.Fatal("decoration value does not decode back to the token")
	}

	// No token: nothing is touched and nothing is observed.
	res = runTracker(t, trackerScenario{
		Href:  "https://customer.com/pricing",
		Links: []string{"https://buy.stripe.com/abc"},
		Tags:  []trackerTag{{Tag: "stripe-pricing-table", Attrs: map[string]string{}}},
	})
	if res.Links[0] != "https://buy.stripe.com/abc" || len(res.Tags[0]) != 0 || res.Observers != 0 {
		t.Fatalf("decorated without a token: %v %v observers=%d", res.Links, res.Tags, res.Observers)
	}
}

// A hostile environment (storage and cookies throwing) never makes the
// script throw into the page, and a second copy of the tag is inert.
func TestTracker_NeverThrows(t *testing.T) {
	tok := trackerToken(t, time.Hour)
	res := runTracker(t, trackerScenario{
		Href:          "https://customer.com/?via=john",
		CookieThrows:  true,
		SessionThrows: true,
		RunTwice:      true,
		Links:         []string{"https://buy.stripe.com/abc"},
	})
	if res.API == nil || res.API.Token != "" {
		t.Fatalf("api=%+v", res.API)
	}
	res = runTracker(t, trackerScenario{
		Href:     "https://customer.com/?via=john",
		RunTwice: true,
		Cookie:   "at_token=" + tok,
	})
	if res.Replaced == nil || len(res.Session) != 1 {
		t.Fatalf("first copy should bounce once: %v %v", res.Replaced, res.Session)
	}
}

// Lemon Squeezy checkout links get the raw token as
// checkout[custom][token]; other Lemon Squeezy pages, a link that already
// carries the field, and look-alike hosts are left alone.
func TestTracker_LemonSqueezyDecoration(t *testing.T) {
	tok := trackerToken(t, 2*time.Hour)
	res := runTracker(t, trackerScenario{
		Href:   "https://customer.com/pricing",
		Cookie: "at_token=" + tok,
		Links: []string{
			"https://acme.lemonsqueezy.com/checkout/buy/9f1c-uuid",
			"https://acme.lemonsqueezy.com/checkout/buy/9f1c-uuid?embed=1",
			"https://acme.lemonsqueezy.com/checkout/buy/x?checkout%5Bcustom%5D%5Btoken%5D=mine",
			"https://acme.lemonsqueezy.com/billing",
			"https://lemonsqueezy.com.evil.test/checkout/buy/x",
		},
	})
	enc := "checkout%5Bcustom%5D%5Btoken%5D=" + url.QueryEscape(tok)
	want := []string{
		"https://acme.lemonsqueezy.com/checkout/buy/9f1c-uuid?" + enc,
		"https://acme.lemonsqueezy.com/checkout/buy/9f1c-uuid?embed=1&" + enc,
		"https://acme.lemonsqueezy.com/checkout/buy/x?checkout%5Bcustom%5D%5Btoken%5D=mine",
		"https://acme.lemonsqueezy.com/billing",
		"https://lemonsqueezy.com.evil.test/checkout/buy/x",
	}
	for i, w := range want {
		if res.Links[i] != w {
			t.Fatalf("link %d = %q, want %q", i, res.Links[i], w)
		}
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
}

// Polar checkout links get the raw token as reference_id (Polar copies it
// into the order's metadata); other Polar pages, a link that already has
// one, and look-alike hosts are left alone.
func TestTracker_PolarDecoration(t *testing.T) {
	tok := trackerToken(t, 2*time.Hour)
	res := runTracker(t, trackerScenario{
		Href:   "https://customer.com/pricing",
		Cookie: "at_token=" + tok,
		Links: []string{
			"https://buy.polar.sh/polar_cl_abc123",
			"https://sandbox-api.polar.sh/v1/checkout-links/polar_cl_abc/redirect",
			"https://buy.polar.sh/polar_cl_abc123?reference_id=mine",
			"https://polar.sh/acme",
			"https://buy.polar.sh.evil.test/polar_cl_x",
		},
	})
	enc := "reference_id=" + url.QueryEscape(tok)
	want := []string{
		"https://buy.polar.sh/polar_cl_abc123?" + enc,
		"https://sandbox-api.polar.sh/v1/checkout-links/polar_cl_abc/redirect?" + enc,
		"https://buy.polar.sh/polar_cl_abc123?reference_id=mine",
		"https://polar.sh/acme",
		"https://buy.polar.sh.evil.test/polar_cl_x",
	}
	for i, w := range want {
		if res.Links[i] != w {
			t.Fatalf("link %d = %q, want %q", i, res.Links[i], w)
		}
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
}
