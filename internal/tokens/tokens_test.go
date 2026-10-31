package tokens

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedNow gives deterministic exp values in roundtrip tests.
var fixedNow = time.Unix(1_700_000_000, 0)

// hmacSHA256ForTest forges a signature directly, to exercise the
// "MAC valid but payload not JSON" path without relying on string mutation.
func hmacSHA256ForTest(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := New([]byte("0123456789abcdef0123456789abcdef")) // 32 bytes
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return fixedNow }
	return s
}

func TestRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	p, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.ClickID != "abc123" || p.AffiliateID != 42 {
		t.Fatalf("got %+v, want click_id=abc123 affiliate_id=42", p)
	}
	if want := fixedNow.Add(time.Hour).Unix(); p.Exp != want {
		t.Fatalf("exp = %d, want %d", p.Exp, want)
	}
}

func TestPayloadIsCompactJSON(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(tok, ".", 2)[0])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if strings.ContainsAny(string(raw), " \t\n\r") {
		t.Fatalf("payload JSON has whitespace: %q", raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	for _, k := range []string{"click_id", "affiliate_id", "exp"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("payload missing key %q: %q", k, raw)
		}
	}
}

// TestTamperSignatureFlips flips EVERY byte of the signature (both edges and
// interior) and requires rejection of each variant.
func TestTamperSignatureFlips(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.SplitN(tok, ".", 2)
	sig, payload := parts[1], parts[0]

	flipped := 0
	for i := 0; i < len(sig); i++ {
		for _, sub := range []byte{0x00, 0xFF} {
			b := []byte(sig)
			b[i] ^= sub
			if string(b) == sig {
				continue // XOR was a no-op for this char; skip
			}
			flipped++
			if _, err := s.Parse(payload + "." + string(b)); !errors.Is(err, ErrMalformed) {
				t.Fatalf("sig byte %d ^0x%02x accepted (err=%v)", i, sub, err)
			}
		}
	}
	if flipped == 0 {
		t.Fatal("no signature flips were actually exercised")
	}
}

// TestTamperPayloadFlips flips every byte position of the payload (including
// the JSON structure and the boundary bytes) and requires rejection. The MAC
// must cover the entire payload, so every flip breaks the token.
func TestTamperPayloadFlips(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.SplitN(tok, ".", 2)
	payload, sig := parts[0], parts[1]

	flipped := 0
	for i := 0; i < len(payload); i++ {
		for _, sub := range []byte{0x01, 0x20} {
			b := []byte(payload)
			b[i] ^= sub
			if string(b) == payload {
				continue
			}
			flipped++
			// A flipped base64 char may decode to a payload with different
			// length; the MAC covers bytes not chars, but any change to the
			// base64 segment changes the recomputed MAC input, so all must fail.
			if _, err := s.Parse(string(b) + "." + sig); !errors.Is(err, ErrMalformed) {
				t.Fatalf("payload byte %d ^0x%02x accepted (err=%v)", i, sub, err)
			}
		}
	}
	if flipped == 0 {
		t.Fatal("no payload flips were actually exercised")
	}
}

func TestExpired(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign("abc123", 42, -time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s.Parse(tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired token: got %v, want ErrExpired", err)
	}

	// Exactly-at-expiry is expired (inclusive cutoff).
	tok, err = s.Sign("abc123", 42, 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s.Parse(tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("zero-ttl token: got %v, want ErrExpired", err)
	}

	// One second of life remains → valid.
	tok, err = s.Sign("abc123", 42, time.Second)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s.Parse(tok); err != nil {
		t.Fatalf("1s-ttl token rejected: %v", err)
	}
}

func TestDifferentSecretRejected(t *testing.T) {
	s1, s2 := newTestSigner(t), newTestSigner(t)
	s2.secret = []byte("ffffffffffffffffffffffffffffffff") // still 32 bytes
	tok, err := s1.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s2.Parse(tok); !errors.Is(err, ErrMalformed) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
}

func TestMalformedShapes(t *testing.T) {
	s := newTestSigner(t)
	valid, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.SplitN(valid, ".", 2)

	cases := map[string]string{
		"empty":            "",
		"no dot":           parts[0],
		"trailing dot":     parts[0] + ".",
		"leading dot":      "." + parts[1],
		"two dots":         parts[0] + ".." + parts[1],
		"bad b64 payload":  "!!!." + parts[1],
		"bad b64 sig":      parts[0] + ".!!!",
		"truncated sig":    parts[0] + "." + parts[1][:len(parts[1])-1],
		"extended sig":     parts[0] + "." + parts[1] + "A",
		"swapped segments": parts[1] + "." + parts[0],
		"garbage":          "garbage",
	}
	for name, tok := range cases {
		if _, err := s.Parse(tok); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", name, err)
		}
	}

	// Valid MAC over non-JSON bytes (forged via the secret itself): the MAC
	// passes but the payload is garbage → still ErrMalformed, never a panic.
	forged := base64.RawURLEncoding.EncodeToString([]byte("not json at all"))
	mac := hmacSHA256ForTest(s.secret, []byte("not json at all"))
	if _, err := s.Parse(forged + "." + mac); !errors.Is(err, ErrMalformed) {
		t.Errorf("valid-mac bad-json: got %v, want ErrMalformed", err)
	}
}

func TestSecretEnforcement(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("New accepted a 5-byte secret")
	}
	if _, err := New(make([]byte, 31)); err == nil {
		t.Fatal("New accepted a 31-byte secret")
	}
	if _, err := New(make([]byte, 32)); err != nil {
		t.Fatalf("New rejected a 32-byte secret: %v", err)
	}
	if _, err := NewRandomSecret(31); err == nil {
		t.Fatal("NewRandomSecret accepted n=31")
	}
	b1, err := NewRandomSecret(32)
	if err != nil {
		t.Fatalf("NewRandomSecret(32): %v", err)
	}
	b2, err := NewRandomSecret(32)
	if err != nil {
		t.Fatalf("NewRandomSecret(32): %v", err)
	}
	if string(b1) == string(b2) {
		t.Fatal("two random secrets are identical")
	}
	// A random secret must be usable.
	if _, err := New(b1); err != nil {
		t.Fatalf("New(random secret): %v", err)
	}
}

func TestSecretCopiedNotAliased(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s, err := New(secret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range secret {
		secret[i] = 0
	}
	tok, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s.Parse(tok); err != nil {
		t.Fatalf("Parse after mutating caller's secret slice: %v", err)
	}
}

// TestRoundTripThroughput is the bench-style gate: 10,000 sign+parse
// roundtrips must complete well inside a fixed budget. Skipped under
// -race: race instrumentation multiplies crypto cost ~20x, which
// measures the sanitizer, not the package.
//
// Robustness: a raw wall-clock budget measured cold on a busy CI runner
// flakes (observed 102ms vs 100ms while lint jobs saturated the CPU —
// same defect class as the TestBench_JSON fix in dc7b12b). So: (1) a
// warm-up pass absorbs allocator/page-cache cold start, (2) the budget
// keeps ~15x headroom over warm steady-state (~16ms/10k on an M-series
// laptop) — plenty to catch the regressions this gate exists for (a
// wrong or misused crypto primitive is 10-1000x slower), while immune
// to a shared runner's noisy neighbourhood.
func TestRoundTripThroughput(t *testing.T) {
	if raceEnabled {
		t.Skip("throughput budget not meaningful under -race")
	}
	s := newTestSigner(t)
	const n = 10_000
	roundtrip := func(count int) time.Duration {
		start := time.Now()
		for i := 0; i < count; i++ {
			tok, err := s.Sign(fmt.Sprintf("click-%d", i), int64(i), time.Hour)
			if err != nil {
				t.Fatalf("Sign %d: %v", i, err)
			}
			if _, err := s.Parse(tok); err != nil {
				t.Fatalf("Parse %d: %v", i, err)
			}
		}
		return time.Since(start)
	}
	roundtrip(n / 2) // warm-up: allocator, page cache, code layout
	// Best of 3: load on a shared machine only ever adds time, so the
	// fastest pass is the code's own speed (one pass at load average 31
	// took 564ms; the best of three never comes near the budget).
	d := roundtrip(n)
	for range 2 {
		d = min(d, roundtrip(n))
	}
	const budget = 250 * time.Millisecond
	if d >= budget {
		t.Fatalf("%d roundtrips took %v, budget %v", n, d, budget)
	}
	t.Logf("%d roundtrips in %v (budget %v)", n, d, budget)
}

func TestWriteReadCookie(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign("abc123", 42, time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	w := httptest.NewRecorder()
	WriteCookie(w, "aff", tok, 2*time.Hour, "example.com", true)
	resp := w.Result()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "aff" || c.Value != tok {
		t.Fatalf("cookie = %s=%s, want aff=%s", c.Name, c.Value, "aff")
	}
	if !c.HttpOnly {
		t.Error("cookie not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if !c.Secure {
		t.Error("cookie not Secure")
	}
	if c.Domain != "example.com" {
		t.Errorf("Domain = %q, want example.com", c.Domain)
	}
	if c.MaxAge != 7200 {
		t.Errorf("MaxAge = %d, want 7200", c.MaxAge)
	}
	// Parse the token back out of the Set-Cookie header end-to-end.
	if p, err := s.Parse(c.Value); err != nil || p.ClickID != "abc123" {
		t.Fatalf("token from cookie failed Parse: %+v %v", p, err)
	}

	// ReadCookie: present, absent, empty-value.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Cookie", resp.Header.Get("Set-Cookie"))
	if got := ReadCookie(r, "aff"); got != tok {
		t.Errorf("ReadCookie = %q, want the token", got)
	}
	if got := ReadCookie(r, "missing"); got != "" {
		t.Errorf("ReadCookie(missing) = %q, want empty", got)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Cookie", "aff=")
	if got := ReadCookie(r2, "aff"); got != "" {
		t.Errorf("ReadCookie(empty) = %q, want empty", got)
	}
}

func TestWriteCookieHostOnlyAndInsecure(t *testing.T) {
	w := httptest.NewRecorder()
	WriteCookie(w, "aff", "tok", time.Hour, "", false)
	c := w.Result().Cookies()[0]
	if c.Domain != "" {
		t.Errorf("Domain = %q, want empty (host-only)", c.Domain)
	}
	if c.Secure {
		t.Error("Secure set despite secure=false")
	}
}

// client_reference_id round trip: Stripe allows only [A-Za-z0-9_-], and
// base64url payloads can contain "_" themselves, so decoding must split by
// the signature's fixed length, never at the first "_". ('?' in the click
// id forces "_" into the payload's base64 at some alignments.)
func TestClientReferenceIDRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	underscored := 0
	for i := 0; i < 200; i++ {
		tok, err := s.Sign(strings.Repeat("?", i%3+1)+fmt.Sprint(i), int64(i*7919+1), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tok[:strings.IndexByte(tok, '.')], "_") {
			underscored++
		}
		ref, ok := ClientReferenceID(tok)
		if !ok {
			t.Fatalf("encode %q failed", tok)
		}
		if !strings.HasPrefix(ref, ClientReferencePrefix) || strings.Contains(ref, ".") || len(ref) > 200 {
			t.Fatalf("ref %q is not a valid client_reference_id", ref)
		}
		back, ok := FromClientReferenceID(ref)
		if !ok || back != tok {
			t.Fatalf("decode(%q) = %q, %v; want %q", ref, back, ok, tok)
		}
		if _, err := s.Parse(back); err != nil {
			t.Fatalf("decoded token does not verify: %v", err)
		}
	}
	if underscored == 0 {
		t.Fatal("fixture never produced a payload with '_' (the case position-split decoding exists for)")
	}
}

// Anything that is not an at1_ token passes through as not-ours: the
// Stripe account may use client_reference_id for its own ids.
func TestFromClientReferenceIDRejectsForeign(t *testing.T) {
	sig := strings.Repeat("A", 43)
	for _, ref := range []string{
		"",
		"user_42",
		"cus_123",
		"at1_",
		"at1__" + sig,         // empty payload
		"at1_abc" + sig,       // no separator before the signature
		"at1_abc." + sig,      // "." is not a Stripe-legal char
		"AT1_abc_" + sig,      // prefix is case-sensitive
		"at1_ab c_" + sig,     // space
		"at1_abc_" + sig[:42], // signature too short: separator misplaced
		"at1_" + strings.Repeat("a", 200) + "_" + sig, // over Stripe's 200 chars
	} {
		if tok, ok := FromClientReferenceID(ref); ok {
			t.Errorf("FromClientReferenceID(%q) = %q, accepted", ref, tok)
		}
	}
	if _, ok := ClientReferenceID("not-a-token"); ok {
		t.Error("ClientReferenceID accepted a non-token")
	}
	if _, ok := ClientReferenceID("abc." + sig[:40]); ok {
		t.Error("ClientReferenceID accepted a short signature")
	}
}

// A tampered at1_ value still decodes (decoding is only a re-shape) but
// never verifies: the HMAC check stays with Parse.
func TestClientReferenceIDTamperedFailsParse(t *testing.T) {
	s := newTestSigner(t)
	tok, _ := s.Sign("abc123", 42, time.Hour)
	ref, _ := ClientReferenceID(tok)
	b := []byte(ref)
	i := len(ClientReferencePrefix) + 3
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	back, ok := FromClientReferenceID(string(b))
	if !ok {
		t.Fatal("tampered ref should still decode")
	}
	if _, err := s.Parse(back); !errors.Is(err, ErrMalformed) {
		t.Fatalf("tampered token Parse err = %v, want ErrMalformed", err)
	}
}
