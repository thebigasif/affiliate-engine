package postbacks_test

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/postbacks"
)

// ssrfEnv builds an emitter pointed at a callback that counts hits. The
// callback server is reachable but represents "the internet" — the guard
// under test is whether INTERNAL targets are refused before dial.
func ssrfEnv(t *testing.T) (*postbacks.Emitter, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ssrf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := postbacks.NewEmitter(db)
	// Tests run against 127.0.0.1 httptest servers; the guard under test is
	// the redirect HOP check (public redirector -> internal target).
	e.AllowLocalhost = true
	_ = e.IsSafeTargetForTest("http://localhost/warm") // latch the knob
	return e, &hits
}

// SSRF1: every textual loopback/private encoding is rejected by safeTarget
// (the cheap pre-filter) — decimal, hex, octal, 4-in-6, ULA, metadata.
func TestSSRF_TextualEncodings(t *testing.T) {
	e, _ := ssrfEnv(t)
	e.AllowLocalhost = false
	_ = e.IsSafeTargetForTest("http://93.184.216.34/strict") // latch strict
	bad := []string{
		"http://2130706433/hook",           // decimal 127.0.0.1
		"http://0x7f000001/hook",           // hex
		"http://0177.0.0.1/hook",           // octal
		"http://127.0.0.1/hook",            // literal
		"http://[::ffff:127.0.0.1]/hook",   // 4-in-6
		"http://[::1]/hook",                // v6 loopback
		"http://[fd12::1]/hook",            // ULA
		"http://10.1.2.3/hook",             // RFC1918
		"http://172.16.0.9/hook",           // RFC1918
		"http://192.168.1.4/hook",          // RFC1918
		"http://169.254.169.254/latest/",   // cloud metadata
		"http://metadata.google.internal/", // gcp metadata
		"http://localhost/hook",
		"ftp://example.com/hook", // scheme
		"not a url",
	}
	for _, u := range bad {
		if e.IsSafeTargetForTest(u) {
			t.Errorf("bypass accepted: %s", u)
		}
	}
}

// SSRF2: dial-time enforcement — a request to a literal internal IP never
// opens a connection (blocked in DialContext, not just pre-filter).
func TestSSRF_DialTimeBlock(t *testing.T) {
	e, _ := ssrfEnv(t)
	e.AllowLocalhost = false
	_ = e.IsSafeTargetForTest("http://93.184.216.34/warm")
	done := make(chan error, 1)
	go func() {
		done <- e.FireForTest(context.Background(), "http://127.0.0.1:1/hook")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dial to loopback succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dial hung")
	}
}

// SSRF3: redirects to internal targets are refused per-hop.
func TestSSRF_RedirectToInternal(t *testing.T) {
	e, _ := ssrfEnv(t)
	// Latch stays ON (the redirector + callback are loopback test servers);
	// the REDIRECT HOP check is unconditional — pivoting to the metadata
	// IP must be refused even when the initial dial target is allowed.
	// A public redirector that 302s to the metadata IP.
	var redirectHits int64
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&redirectHits, 1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	err := e.FireForTest(context.Background(), redirector.URL+"/start")
	if err == nil {
		t.Fatal("redirect to internal target allowed")
	}
	if atomic.LoadInt64(&redirectHits) != 1 {
		t.Fatalf("redirector hit %d times", redirectHits)
	}
}

// SSRF4: special-purpose ranges net.IP's predicates miss — CGNAT, "this
// network", benchmarking, IETF assignments, class E / broadcast — and v6
// spellings that translate to an internal v4 (NAT64, 6to4, v4-compatible)
// are refused; public addresses in the same encodings still pass.
func TestSSRF_SpecialPurposeRanges(t *testing.T) {
	e, _ := ssrfEnv(t)
	e.AllowLocalhost = false
	bad := []string{
		"http://100.64.0.1/hook",           // CGNAT shared address space
		"http://100.127.255.254/hook",      // CGNAT upper edge
		"http://0.1.2.3/hook",              // "this network"
		"http://198.18.0.1/hook",           // benchmarking
		"http://198.19.255.1/hook",         // benchmarking upper half
		"http://192.0.0.8/hook",            // IETF protocol assignments
		"http://240.0.0.1/hook",            // class E reserved
		"http://255.255.255.255/hook",      // limited broadcast
		"http://[64:ff9b::7f00:1]/hook",    // NAT64 of 127.0.0.1
		"http://[64:ff9b::a9fe:a9fe]/hook", // NAT64 of 169.254.169.254
		"http://[64:ff9b:1::a00:1]/hook",   // NAT64 local-use prefix
		"http://[2002:7f00:1::1]/hook",     // 6to4 of 127.0.0.1
		"http://[2002:a00:1::1]/hook",      // 6to4 of 10.0.0.1
		"http://[fc00::1]/hook",            // ULA, fc00::/7 lower half
		"http://[fe80::1]/hook",            // link-local
		"http://[::7f00:1]/hook",           // v4-compatible 127.0.0.1
	}
	for _, u := range bad {
		if e.IsSafeTargetForTest(u) {
			t.Errorf("special-purpose target accepted: %s", u)
		}
	}
	good := []string{
		"http://93.184.216.34/hook",
		"http://[64:ff9b::5db8:d822]/hook", // NAT64 of a public v4
		"http://[2002:5db8:d822::1]/hook",  // 6to4 of a public v4
		"http://[2606:4700:4700::1111]/hook",
	}
	for _, u := range good {
		if !e.IsSafeTargetForTest(u) {
			t.Errorf("public target refused: %s", u)
		}
	}
}

// SSRF5: resolver failure FAILS CLOSED. .invalid never resolves (RFC
// 6761): the pre-filter must refuse it (the old code returned true and
// deferred to a second, attacker-influenced resolution at dial time), and
// the dial path must error instead of connecting.
func TestSSRF_ResolverFailureBlocked(t *testing.T) {
	e, hits := ssrfEnv(t)
	e.AllowLocalhost = false
	if e.IsSafeTargetForTest("http://ssrf-probe.invalid/hook") {
		t.Fatal("unresolvable host accepted by the pre-filter (must fail closed)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.FireForTest(ctx, "http://ssrf-probe.invalid/hook"); err == nil {
		t.Fatal("dial to an unresolvable host succeeded")
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Fatalf("callback hit %d times", n)
	}
}

// SSRF6: the dial path validates what it connects to without relying on
// the pre-filter (FireForTest bypasses safeTarget): a loopback NAME and a
// special-purpose literal are refused at dial time.
func TestSSRF_DialTimeNameAndSpecialRange(t *testing.T) {
	e, _ := ssrfEnv(t)
	e.AllowLocalhost = false
	for _, target := range []string{
		"http://localhost:1/hook",
		"http://100.64.0.1:1/hook",
		"http://[64:ff9b::7f00:1]:1/hook",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := e.FireForTest(ctx, target)
		cancel()
		if err == nil {
			t.Errorf("dial to %s succeeded", target)
		}
	}
}

var _ = net.Dial
