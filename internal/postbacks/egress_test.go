package postbacks

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeResolver answers from a script: call i gets answers[min(i, last)].
type fakeResolver struct {
	mu      sync.Mutex
	calls   int
	answers [][]string
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	f.calls++
	var out []net.IPAddr
	for _, s := range f.answers[i] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func loopbackOnly(ip net.IP) bool { return ip.IsLoopback() }

func tlsFor(srv *httptest.Server) *tls.Config {
	return srv.Client().Transport.(*http.Transport).TLSClientConfig
}

// Every internal range is refused at save time and at dial time.
func TestEgressBlocksInternalRanges(t *testing.T) {
	ctx := context.Background()
	c := NewEgressClient(EgressOptions{})
	for _, target := range []string{
		"https://127.0.0.1/x", "https://10.0.0.5/x", "https://172.16.3.4/x",
		"https://192.168.1.1/x", "https://169.254.169.254/latest/meta-data",
		"https://100.64.0.1/x", "https://0.0.0.0/x", "https://[::1]/x",
		"https://[fe80::1]/x", "https://[fd00::1]/x", "https://[::ffff:127.0.0.1]/x",
		"https://[64:ff9b::7f00:1]/x", "https://2130706433/x", "https://0x7f000001/x",
		"https://localhost/x", "https://metadata.google.internal/x", "https://224.0.0.1/x",
	} {
		if err := CheckEgressURL(ctx, EgressOptions{}, target); err == nil {
			t.Errorf("save-time check accepted %s", target)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("dial accepted %s", target)
			continue
		}
		if !errors.Is(err, ErrEgressBlocked) {
			t.Errorf("%s: want ErrEgressBlocked, got %v", target, err)
		}
	}
}

// Plain http, userinfo and backslash tricks are refused.
func TestEgressHTTPSOnly(t *testing.T) {
	ctx := context.Background()
	o := EgressOptions{Resolver: &fakeResolver{answers: [][]string{{"93.184.216.34"}}}}
	for _, bad := range []string{
		"http://hooks.example.com/x", "ftp://hooks.example.com/x",
		"https://user:pw@hooks.example.com/x", "https://hooks.example.com\\@127.0.0.1/x", "/relative",
	} {
		if err := CheckEgressURL(ctx, o, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := CheckEgressURL(ctx, o, "https://hooks.example.com/x"); err != nil {
		t.Fatalf("public https refused: %v", err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := NewEgressClient(EgressOptions{AllowIP: loopbackOnly})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if resp, err := c.Do(req); err == nil || !errors.Is(err, ErrEgressScheme) {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatalf("http delivery: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("http target was reached")
	}
}

// A permitted host that answers 302 to an internal address is never
// followed: the hop target receives nothing.
func TestEgressRedirectToLoopbackRefused(t *testing.T) {
	ctx := context.Background()
	var internalHits atomic.Int32
	internal := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { internalHits.Add(1) }))
	defer internal.Close()
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
	}))
	defer front.Close()

	c := NewEgressClient(EgressOptions{AllowIP: loopbackOnly, TLSConfig: tlsFor(front)})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequestWithContext(ctx, method, front.URL, strings.NewReader("{}"))
		resp, err := c.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrEgressRedirect) {
			t.Fatalf("%s: redirect was not refused: %v", method, err)
		}
	}
	if internalHits.Load() != 0 {
		t.Fatalf("redirect target reached %d times", internalHits.Load())
	}
}

// DNS rebinding: the name answers a public address when the merchant
// saves it, then 127.0.0.1 when the dispatcher dials. The dial-time
// check refuses it and the loopback listener sees nothing.
func TestEgressDNSRebind(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))

	res := &fakeResolver{answers: [][]string{{"93.184.216.34"}, {"127.0.0.1"}}}
	o := EgressOptions{Resolver: res, TLSConfig: tlsFor(srv)}
	target := "https://rebind.example.com:" + port + "/hook"
	if err := CheckEgressURL(ctx, o, target); err != nil {
		t.Fatalf("first (public) answer refused: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
	resp, err := NewEgressClient(o).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("rebound answer not blocked: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("loopback listener reached after rebind")
	}

	// A mixed answer (one public, one internal) is refused as a whole.
	res2 := &fakeResolver{answers: [][]string{{"93.184.216.34", "10.1.2.3"}}}
	if err := CheckEgressURL(ctx, EgressOptions{Resolver: res2}, target); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("mixed answer accepted: %v", err)
	}
}

// The body cap is 64 KB.
func TestEgressResponseCap(t *testing.T) {
	big := strings.NewReader(strings.Repeat("a", 1<<20))
	b, err := ReadCapped(big)
	if err != nil || len(b) != MaxEgressResponse {
		t.Fatalf("read %d bytes, err %v", len(b), err)
	}
}
