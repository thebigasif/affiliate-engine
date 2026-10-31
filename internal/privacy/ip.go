// Package privacy implements NFR-7's subject-rights half: write-time IP
// anonymization and GDPR export/erasure for data subjects.
//
// Erasure design constraint (the reason this is not a DELETE cascade):
// the ledger is append-only and every commission/payout row is a booked
// financial fact. Erasing a data subject therefore SCRUBS identity
// fields in place and keeps the money rows — the books stay balanced,
// the replay-dedupe unique indexes stay intact, and the pseudonymous id
// remains for accounting-retention obligations (GDPR Art. 6(1)(c) via
// Art. 20(3) exceptions; the operator's local retention law governs the
// timeline).
package privacy

import (
	"fmt"
	"net"
	"strings"
)

// IPMode selects write-time IP anonymization on the click path. The raw
// IP is never stored — only keyed identity hashes (truncated HMAC-SHA256
// under the deployment's hash key, see fingerprint.SetHashKey; an unkeyed
// hash of an IPv4 address is reversible by enumerating all 2^32
// addresses) — so this controls how much entropy enters the hash input.
type IPMode int

const (
	// IPModeOff stores the full-precision IP's hash (default; maximum
	// fraud-rule and unique-counting fidelity).
	IPModeOff IPMode = iota
	// IPModeTrunc24 truncates IPv4 to /24 and IPv6 to /48 before
	// hashing — the classic GDPR-safe pair. Unique counting degrades
	// to subnet granularity; velocity rules see the same.
	IPModeTrunc24
)

// ParseIPMode maps AE_IP_MODE values ("off", "trunc24") to a mode.
// Unknown values are an error — a privacy control must fail loud, not
// silently default.
func ParseIPMode(s string) (IPMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "full":
		return IPModeOff, nil
	case "trunc24", "trunc", "anonymize":
		return IPModeTrunc24, nil
	default:
		return IPModeOff, fmt.Errorf("privacy: unknown AE_IP_MODE %q (want off|trunc24)", s)
	}
}

// AnonymizeIP applies the mode to a host string. Unparseable input
// passes through unchanged (fail-open, mirroring the bot classifier:
// the redirect must never break on a malformed RemoteAddr; the output
// feeds a hash either way).
func AnonymizeIP(m IPMode, ip string) string {
	if m == IPModeOff {
		return ip
	}
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String()
	}
	// IPv6: keep the first 3 groups (/48), zero the rest.
	v6 := p.To16()
	out := make(net.IP, 16)
	copy(out, v6[:6])
	return out.String()
}
