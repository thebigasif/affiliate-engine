package httpapi

import (
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// hstsValue is the Strict-Transport-Security the engine sends on HTTPS
// requests: 180 days, this host only. includeSubDomains and preload are
// left out on purpose: the engine often runs on one subdomain of a domain
// whose other hosts it does not control.
const hstsValue = "max-age=15552000"

// HSTS adds Strict-Transport-Security to every response of a request that
// reached us over HTTPS: directly, or through a trusted proxy that says so
// with X-Forwarded-Proto (the same rule as cookie Secure). Plain HTTP never
// gets it, so a local setup on http://localhost keeps working. on=false
// turns it off (AE_HSTS=0, see HSTSFromEnv), for a proxy that sets its own.
func HSTS(next http.Handler, trusted []netip.Prefix, on bool) http.Handler {
	if !on {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestIsHTTPS(r, trusted) {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

// HSTSFromEnv reports whether HSTS is on: yes unless AE_HSTS is 0, false
// or off.
func HSTSFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AE_HSTS"))) {
	case "0", "false", "off":
		return false
	}
	return true
}

// apiHeaders sets the two protective headers the HTML pages already send
// on every /api/v1 response: no MIME sniffing, and no Referer leaving the
// page (API URLs can carry ids). A handler may still override either.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// WebhookPerIPPerMinute caps billing webhook requests from one client IP
// per minute. Billing tools send bursts (a bulk refund or a migration can
// replay thousands of events) from a few addresses, so the limit is set
// far above any real burst: 3000 a minute is 50 a second, sustained, from
// one address. It only stops a flood. A refused request gets 429 and
// Retry-After, which every supported billing tool retries later.
const WebhookPerIPPerMinute = 3000

// NewIPLimiter returns a wrapper that allows perMinute requests per client
// IP (ClientIP: trusted proxies honoured) per minute across every handler
// it wraps, and answers 429 beyond that.
func NewIPLimiter(perMinute int, trusted []netip.Prefix) func(http.HandlerFunc) http.HandlerFunc {
	rl := newRateLimiter(perMinute)
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow("ip:" + ClientIP(r, trusted)) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next(w, r)
		}
	}
}
