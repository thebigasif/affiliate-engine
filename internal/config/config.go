// Package config loads runtime configuration from environment + file,
// with secure defaults for every knob.
package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// Config is the full runtime configuration.
type Config struct {
	// Database: either "sqlite:<path>" or "postgres:<dsn>"
	Database string

	// HTTP server
	Listen string // default ":8080"

	// Secrets
	TokenSecret []byte // HMAC secret for attribution tokens; required

	// Ledger safety
	LedgerSyncFull bool // synchronous=FULL on the ledger connection (default true)

	// Click pipeline
	ClickBatchMax int // default 500
	ClickFlushMs  int // default 150

	// TrustedProxies (AE_TRUSTED_PROXIES): only peers inside these ranges
	// may supply X-Forwarded-For / X-Forwarded-Proto / CDN geo headers.
	// Empty = no proxy is trusted; the TCP peer is the client.
	TrustedProxies []netip.Prefix
}

// Defaults returns the production-default configuration.
func Defaults() Config {
	return Config{
		Database:       "sqlite:affiliate-engine.db",
		Listen:         ":8080",
		LedgerSyncFull: true,
		ClickBatchMax:  500,
		ClickFlushMs:   150,
	}
}

// ParseTrustedProxies parses AE_TRUSTED_PROXIES: a comma-separated list of
// CIDRs ("10.0.0.0/8") or bare addresses ("203.0.113.7" = a /32 or /128).
// Blank entries are ignored; any malformed entry is an error (a proxy
// trust setting must never be silently narrowed or widened).
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("AE_TRUSTED_PROXIES: %q: %w", part, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("AE_TRUSTED_PROXIES: %q: %w", part, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
