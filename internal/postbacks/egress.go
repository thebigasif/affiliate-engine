package postbacks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The egress client is the one way the Shopify app calls a URL that a
// merchant or affiliate supplied (SECURITY-AND-PRIVACY 7.7): outbound
// webhooks (FR-113), integrations and every later postback-shaped call.
//
//   - HTTPS only, on every request the client sends.
//   - Redirects are off: a 3xx is returned as the response and never
//     followed, so a public host cannot bounce the call to 127.0.0.1 or
//     the metadata service.
//   - The host is resolved once at dial time and every answer must be a
//     public address (loopback, RFC 1918, link-local including
//     169.254.169.254, CGNAT 100.64.0.0/10, multicast, 0.0.0.0/8, IPv6
//     loopback, link-local, unique local and v4-embedding forms are
//     refused). The socket then connects to the validated IP literal and
//     net.Dialer.Control re-checks the address actually dialed, so a
//     second, rebinding DNS answer can never be used.
//   - Callers read responses through ReadCapped (64 KB).

// MaxEgressResponse caps how much of a response body is read.
const MaxEgressResponse = 64 << 10

// Errors the egress client returns.
var (
	ErrEgressScheme   = errors.New("egress: https only")
	ErrEgressRedirect = errors.New("egress: redirects are not followed")
	ErrEgressBlocked  = errors.New("egress: internal address blocked")
	ErrEgressURL      = errors.New("egress: invalid url")
)

// Resolver resolves a host name (net.DefaultResolver in production; a
// fake in the DNS-rebinding tests).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// EgressOptions configures NewEgressClient. The zero value is the
// production setting.
type EgressOptions struct {
	// Timeout bounds the whole request (default 10s).
	Timeout time.Duration
	// Resolver answers host lookups (default net.DefaultResolver).
	Resolver Resolver
	// AllowIP admits extra addresses on top of the public ones. TESTS
	// ONLY (httptest binds 127.0.0.1); production wiring never sets it.
	AllowIP func(net.IP) bool
	// TLSConfig overrides the TLS settings. TESTS ONLY (trusting the
	// httptest certificate). MinVersion is forced to TLS 1.2.
	TLSConfig *tls.Config
}

func (o EgressOptions) resolver() Resolver {
	if o.Resolver != nil {
		return o.Resolver
	}
	return net.DefaultResolver
}

func (o EgressOptions) allowed(ip net.IP) bool {
	if isPublicIP(ip) {
		return true
	}
	return o.AllowIP != nil && o.AllowIP(ip)
}

// NewEgressClient builds the guarded client.
func NewEgressClient(o EgressOptions) *http.Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.TLSConfig != nil {
		tlsConf = o.TLSConfig.Clone()
		if tlsConf.MinVersion < tls.VersionTLS12 {
			tlsConf.MinVersion = tls.VersionTLS12
		}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return egressDial(ctx, o, network, addr)
		},
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          32,
		IdleConnTimeout:       30 * time.Second,
		// No proxy: an environment proxy would dial on our behalf and
		// bypass the address check.
		Proxy: nil,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: httpsOnly{next: tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrEgressRedirect
		},
	}
}

// httpsOnly refuses any request that is not https before it dials.
type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.Scheme != "https" {
		return nil, ErrEgressScheme
	}
	return h.next.RoundTrip(r)
}

// egressDial resolves once, checks every answer, then connects to the
// validated literal; Control re-checks the dialed address.
func egressDial(ctx context.Context, o EgressOptions, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	switch {
	case net.ParseIP(host) != nil:
		ips = []net.IP{net.ParseIP(host)}
	case parseAltIPv4(host) != nil:
		ips = []net.IP{parseAltIPv4(host)}
	default:
		if blockedHostname(host) {
			return nil, ErrEgressBlocked
		}
		addrs, err := o.resolver().LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("egress: resolve: %w", err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: no addresses")
	}
	for _, ip := range ips {
		if !o.allowed(ip) {
			return nil, ErrEgressBlocked
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		h, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(h)
		if ip == nil || !o.allowed(ip) {
			return ErrEgressBlocked
		}
		return nil
	}}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// CheckEgressURL is the save-time check for a merchant-supplied URL:
// absolute https, a host, no userinfo, and a host whose every current
// address is public. The dial-time check stays the enforcement (the
// answer can change later); this only refuses obviously bad targets at
// subscribe time with a clear error.
func CheckEgressURL(ctx context.Context, o EgressOptions, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return ErrEgressURL
	}
	if u.Scheme != "https" {
		return ErrEgressScheme
	}
	if u.User != nil || strings.ContainsAny(raw, "\\") {
		return ErrEgressURL
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !o.allowed(ip) {
			return ErrEgressBlocked
		}
		return nil
	}
	if ip := parseAltIPv4(host); ip != nil {
		if !o.allowed(ip) {
			return ErrEgressBlocked
		}
		return nil
	}
	if blockedHostname(host) {
		return ErrEgressBlocked
	}
	addrs, err := o.resolver().LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return ErrEgressBlocked
	}
	for _, a := range addrs {
		if !o.allowed(a.IP) {
			return ErrEgressBlocked
		}
	}
	return nil
}

// ReadCapped reads at most MaxEgressResponse bytes of a response body
// and drains nothing further.
func ReadCapped(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, MaxEgressResponse))
}
