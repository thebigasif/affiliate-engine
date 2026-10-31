package httpapi

import (
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// ClientIP returns the client address used for rate limiting and click
// identity. The TCP peer (RemoteAddr) is authoritative unless that peer is
// inside a trusted proxy CIDR; only then is X-Forwarded-For consulted,
// walked from the RIGHT (the entries our own proxies appended), skipping
// hops that are themselves trusted proxies. The first untrusted hop is the
// client. A client-supplied (leftmost) XFF value is never believed on its
// own, and with no trusted proxies configured XFF is ignored entirely.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, ok := peerAddr(r)
	if !ok {
		return clientHost(r.RemoteAddr)
	}
	if !inPrefixes(peer, trusted) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		a, ok := parseHop(h)
		if !ok {
			break // garbage in the proxy chain: trust nothing further left
		}
		if !inPrefixes(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

// peerTrusted reports whether the TCP peer is one of our trusted proxies
// (the only case in which proxy-set headers may be believed).
func peerTrusted(r *http.Request, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	peer, ok := peerAddr(r)
	return ok && inPrefixes(peer, trusted)
}

// requestIsHTTPS reports whether the client reached us over TLS: directly,
// or through a trusted proxy that says so via X-Forwarded-Proto.
func requestIsHTTPS(r *http.Request, trusted []netip.Prefix) bool {
	if r.TLS != nil {
		return true
	}
	return peerTrusted(r, trusted) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// SecureCookiesFromEnv reports whether cookies must always carry Secure:
// AE_COOKIE_SECURE=1|true, or an https AE_PUBLIC_URL (the deployment is
// served over TLS even when TLS terminates in front of us).
func SecureCookiesFromEnv() bool {
	if v := os.Getenv("AE_COOKIE_SECURE"); v == "1" || v == "true" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(os.Getenv("AE_PUBLIC_URL"))), "https://")
}

func peerAddr(r *http.Request) (netip.Addr, bool) {
	a, err := netip.ParseAddr(clientHost(r.RemoteAddr))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// parseHop parses one X-Forwarded-For entry: a bare address, or an
// address:port / [v6]:port some proxies emit.
func parseHop(h string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(h); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
