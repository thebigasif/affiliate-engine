package httpapi_test

// /r/ hardening pins: per-IP rate limit, field caps, and proxy-header
// trust (XFF / geo headers only from a trusted proxy peer).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fingerprint"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

func hardenedRedirect(t *testing.T, mutate func(*httpapi.Config)) (*httpapi.RedirectHandler, *db.Handles) {
	t.Helper()
	ctx := context.Background()
	h, err := db.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "test.db"), db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
		VALUES (1, 'asif', TRUE, 'native')`); err != nil {
		t.Fatal(err)
	}
	secret, _ := tokens.NewRandomSecret(32)
	signer, _ := tokens.New(secret)
	committer := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(committer.Close)
	cfg := httpapi.Config{
		Signer:              signer,
		AllowedDestSuffixes: []string{"example.com"},
		FallbackURL:         "https://example.com/",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return httpapi.NewRedirectHandler(h.DB, cfg, committer), h
}

func redirectReq(remote, target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = remote
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
	return req
}

func waitClickRow(t *testing.T, h *db.Handles) (referer, utm, subid, country string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.DB.QueryRow(`SELECT referer, utm_source, subid, country FROM clicks LIMIT 1`).
			Scan(&referer, &utm, &subid, &country); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("click not persisted")
	return
}

// Per-IP limit: over the cap answers 429 (no redirect, no click); other
// IPs are unaffected.
func TestRedirect_RateLimitPerIP(t *testing.T) {
	handler, _ := hardenedRedirect(t, func(c *httpapi.Config) { c.RateLimitPerMin = 2 })
	var codes []int
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, redirectReq("81.2.3.4:1000", "/r/asif?dest=https://example.com/x"))
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusFound {
		t.Fatalf("first request code=%d want 302", codes[0])
	}
	limited := false
	for _, c := range codes {
		if c == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatalf("5 hits with a 2/min limit never answered 429: %v", codes)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, redirectReq("81.2.3.99:1000", "/r/asif?dest=https://example.com/x"))
	if rec.Code != http.StatusFound {
		t.Fatalf("other IP code=%d want 302", rec.Code)
	}
}

// Oversized attacker-controlled fields are capped (UTF-8 safe) before
// they reach the clicks table.
func TestRedirect_FieldCaps(t *testing.T) {
	handler, h := hardenedRedirect(t, nil)
	longUTM := strings.Repeat("u", 1000)
	longSub := strings.Repeat("é", 300) // 600 bytes of two-byte runes
	q := url.Values{}
	q.Set("dest", "https://example.com/x")
	q.Set("utm_source", longUTM)
	q.Set("subid", longSub)
	req := redirectReq("81.2.3.4:1000", "/r/asif?"+q.Encode())
	req.Header.Set("Referer", "https://ref.example/"+strings.Repeat("r", 5000))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("code=%d", rec.Code)
	}
	referer, utm, subid, _ := waitClickRow(t, h)
	if len(referer) > 1024 || len(referer) < 1000 {
		t.Fatalf("referer len=%d want capped at 1024", len(referer))
	}
	if len(utm) != 256 {
		t.Fatalf("utm_source len=%d want 256", len(utm))
	}
	if len(subid) > 256 || !utf8.ValidString(subid) {
		t.Fatalf("subid len=%d valid=%v (must be <=256 and valid UTF-8)", len(subid), utf8.ValidString(subid))
	}
}

// Untrusted peer: spoofed X-Forwarded-For and geo headers are ignored.
func TestRedirect_UntrustedProxyHeadersIgnored(t *testing.T) {
	handler, h := hardenedRedirect(t, nil)
	req := redirectReq("81.2.3.4:1000", "/r/asif?dest=https://example.com/x")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Cf-Ipcountry", "DE")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	got := waitForClickHash(t, h.DB)
	want := fingerprint.NewIdentity(fingerprint.ByIP, "81.2.3.4")
	if string(got) != string(want[:]) {
		t.Fatal("spoofed X-Forwarded-For from an untrusted peer was used as the client IP")
	}
	if _, _, _, country := waitClickRow(t, h); country != "" {
		t.Fatalf("country=%q from an untrusted peer (want empty)", country)
	}
}

// Trusted proxy: the rightmost untrusted XFF hop is the client and the
// edge geo header is honoured.
func TestRedirect_TrustedProxyHeadersHonoured(t *testing.T) {
	handler, h := hardenedRedirect(t, func(c *httpapi.Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	req := redirectReq("10.1.2.3:443", "/r/asif?dest=https://example.com/x")
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	req.Header.Set("Cf-Ipcountry", "de")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	got := waitForClickHash(t, h.DB)
	want := fingerprint.NewIdentity(fingerprint.ByIP, "203.0.113.9")
	if string(got) != string(want[:]) {
		t.Fatal("client IP behind a trusted proxy must be the rightmost untrusted XFF hop")
	}
	if _, _, _, country := waitClickRow(t, h); country != "DE" {
		t.Fatalf("country=%q want DE from the trusted edge", country)
	}
}
