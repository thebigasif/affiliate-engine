// Package fingerprint derives stable click identities and classifies
// bot traffic for the affiliate tracking engine.
//
// Provenance: clean-room implementation of the algorithms described in
// research/data-layer/ (the YellowTDS-style click-uniqueness study and the
// analytics benchmark report). No third-party code was copied; the design
// below is re-expressed with the Go standard library only.
//
// One deliberate deviation from the described design, forced by the
// stdlib-only constraint, is documented here:
//
//   - Identity hashing uses HMAC-SHA256 (crypto/hmac + crypto/sha256)
//     truncated to its first 16 bytes (128 bits) instead of xxHash-128.
//     xxh128 has no stdlib implementation; a truncated keyed SHA-256 keeps
//     the same 128-bit output size used by the study's ip_hash/ua_hash
//     BLOB columns while adding zero dependencies. At a few µs per hash it
//     is far below the measured write-path budget (18.3µs/click total,
//     see research/data-layer/analytics.md §2).
//
// Keying (privacy): the IPv4 space is only 2^32 addresses, so an UNKEYED
// hash of an IP is reversible by brute force in seconds — a stored
// ip_hash would be the raw IP with extra steps. Identities are therefore
// HMAC'd under a process-wide secret key (SetHashKey). Production wiring
// derives the key from the deployment secret (DeriveHashKey) so hashes are
// stable across restarts; until SetHashKey is called a random per-process
// key is used (safe, but hashes then change on every restart).
//
// Hashing detail: the digest is a pure function of the key and the
// length-prefixed parts — the Method itself is NOT mixed in. That is
// intentional: a cookie-less click under ByCookieIPUA then hashes
// identically to the same click under ByIPUA, so "upgrading" a visitor's
// identity when a cookie appears never splits identities across method
// labels.
package fingerprint

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

// Method selects which request attributes build a click identity.
type Method int

const (
	ByIP         Method = iota // ip only
	ByIPUA                     // ip + user-agent
	ByCookie                   // first-party cookie only
	ByCookieIP                 // cookie + ip (fallback to ip when cookie absent)
	ByCookieIPUA               // cookie + ip + user-agent (strongest)
)

// String returns the canonical domain-separator name for the method.
func (m Method) String() string {
	switch m {
	case ByIP:
		return "ip"
	case ByIPUA:
		return "ip+ua"
	case ByCookie:
		return "cookie"
	case ByCookieIP:
		return "cookie+ip"
	case ByCookieIPUA:
		return "cookie+ip+ua"
	default:
		return fmt.Sprintf("method(%d)", int(m))
	}
}

// Hash is a 128-bit identity digest (truncated HMAC-SHA256).
type Hash [16]byte

// Hex returns the hash as a 32-character lowercase hex string.
func (h Hash) Hex() string { return hex.EncodeToString(h[:]) }

// String makes Hash print as its hex form.
func (h Hash) String() string { return h.Hex() }

// hashKey is the process-wide HMAC key for identity hashes. Read on every
// click (redirect goroutines), replaced by SetHashKey: an atomic pointer
// keeps both lock-free and race-free.
var hashKey atomic.Pointer[[]byte]

func init() {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("fingerprint: crypto/rand unavailable: " + err.Error())
	}
	hashKey.Store(&k)
}

// SetHashKey installs the HMAC key for all identity hashes (the slice is
// copied). Call once at startup, before the click path serves traffic,
// with a key derived from the deployment secret (see DeriveHashKey). An
// empty key is ignored: the current key stays in force.
//
// Changing the key changes every identity hash: stored ip_hash/ua_hash
// values stop matching new clicks (unique counts and IP-based fraud
// windows restart from the switch).
func SetHashKey(key []byte) {
	if len(key) == 0 {
		return
	}
	k := make([]byte, len(key))
	copy(k, key)
	hashKey.Store(&k)
}

// DeriveHashKey derives the identity-hash key from a deployment secret:
// HMAC-SHA256(secret, "ip-hash-v1"). The label separates this key from
// every other use of the same secret (e.g. attribution-token signing), and
// the "v1" suffix lets a future rotation derive a fresh key.
func DeriveHashKey(secret []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("ip-hash-v1"))
	return m.Sum(nil)
}

// NewIdentity hashes already-canonicalized parts into a 128-bit identity.
//
// The canonical string length-prefixes every part ("len:part" per part,
// byte length) so part boundaries are unambiguous: parts ("ab","c") and
// ("a","bc") canonicalize differently and can never collide. The string
// is then HMAC-SHA256'd under the package key (SetHashKey) and truncated
// to the first 16 bytes.
//
// The Method is not mixed into the digest: by construction (see
// UniqueHash) the parts alone determine the identity, so a cookie-less
// ByCookieIPUA click digests identically to the same click under ByIPUA.
//
// Callers that want IP/UA/cookie canonicalization should use UniqueHash;
// NewIdentity applies none.
func NewIdentity(method Method, parts ...string) Hash {
	_ = method // not hashed; selects parts at the UniqueHash layer
	var b strings.Builder
	writePart := func(p string) {
		fmt.Fprintf(&b, "%d:%s", len(p), p)
	}
	for _, p := range parts {
		writePart(p)
	}
	mac := hmac.New(sha256.New, *hashKey.Load())
	mac.Write([]byte(b.String()))
	var h Hash
	copy(h[:], mac.Sum(nil)[:16])
	return h
}

// SubnetIdentity hashes the network an address belongs to: the /64 for
// IPv6 (the unit one subscriber is delegated — a single client can rotate
// through 2^64 addresses inside it, so per-address hashes make IPv6
// velocity rules trivially evadable) and the /24 for IPv4. Unparseable
// input hashes as-is. The "net" part domain-separates it from per-address
// identities, so a subnet hash never equals an address hash.
func SubnetIdentity(ip string) Hash {
	p := net.ParseIP(ip)
	if p == nil {
		return NewIdentity(ByIP, "net", ip)
	}
	if v4 := p.To4(); v4 != nil {
		return NewIdentity(ByIP, "net", v4.Mask(net.CIDRMask(24, 32)).String()+"/24")
	}
	return NewIdentity(ByIP, "net", p.To16().Mask(net.CIDRMask(64, 128)).String()+"/64")
}

// UniqueHash derives the click-identity hash for a request using the given
// method, canonicalizing each attribute first:
//
//   - ip: parsed with net.ParseIP; valid addresses (both "1.2.3.4" and its
//     v6-mapped spelling "::ffff:1.2.3.4") are reduced to the 16-byte form
//     and hex-encoded, so all spellings of the same address hash
//     identically. Invalid/unparseable values are used as-is.
//   - ua: lowercased with whitespace runs collapsed to single spaces and
//     trimmed, so cosmetic UA differences do not split identities.
//   - cookie: used verbatim; an empty cookie contributes NO part, which
//     makes ByCookieIP degrade exactly to ByIP and ByCookieIPUA to ByIPUA
//     for cookie-less (first-visit) clicks.
//
// The Method only selects which parts participate; it is not hashed
// (see NewIdentity). Unknown method values hash all three attributes.
func UniqueHash(ip, ua, cookie string, method Method) Hash {
	cip := canonicalIP(ip)
	cua := canonicalUA(ua)

	// The cookie participates only in ByCookie* methods; ByIP/ByIPUA
	// ignore it. When it participates but is empty, it contributes no
	// part, so ByCookieIP degrades to ByIP and ByCookieIPUA to ByIPUA.
	usesCookie := method == ByCookie || method == ByCookieIP || method == ByCookieIPUA ||
		(method != ByIP && method != ByIPUA) // unknown methods: hash everything

	var parts []string
	if usesCookie && cookie != "" {
		parts = append(parts, cookie)
	}
	switch method {
	case ByIP:
		parts = append(parts, cip)
	case ByIPUA:
		parts = append(parts, cip, cua)
	case ByCookie:
		// cookie only (possibly none)
	case ByCookieIP:
		parts = append(parts, cip)
	case ByCookieIPUA:
		parts = append(parts, cip, cua)
	default:
		parts = append(parts, cip, cua)
	}
	return NewIdentity(method, parts...)
}

// canonicalIP reduces an address to a spelling-independent form.
func canonicalIP(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip // not an IP literal; hash the raw string as-is
	}
	return hex.EncodeToString(p.To16()) // 16-byte form: unifies v4 and v4-mapped-v6
}

// canonicalUA lowercases and collapses all whitespace runs to single
// spaces, trimming the result.
func canonicalUA(ua string) string {
	return strings.Join(strings.Fields(strings.ToLower(ua)), " ")
}

// BotClass grades how confidently a request is automated.
type BotClass int

const (
	Human     BotClass = 0 // looks like a real browser user
	Suspected BotClass = 1 // anomalous signals, not conclusive
	Known     BotClass = 2 // matches a known bot/crawler/library signature
)

// String names the class for logs and metrics.
func (c BotClass) String() string {
	switch c {
	case Human:
		return "human"
	case Suspected:
		return "suspected"
	case Known:
		return "known"
	default:
		return fmt.Sprintf("botclass(%d)", int(c))
	}
}

// knownBotTokens are lowercase substrings that identify known crawlers,
// fetchers, and HTTP libraries in a User-Agent.
var knownBotTokens = []string{
	"googlebot", "bingbot", "slurp", "duckduckbot", "baiduspider",
	"yandexbot", "facebookexternalhit", "twitterbot", "linkedinbot",
	"curl", "wget", "python-requests", "python-urllib", "go-http-client",
	"java/", "okhttp", "libwww", "httpclient", "node-fetch", "scrapy",
	"headlesschrome", "phantomjs",
}

// Classify grades a request from its User-Agent and header presence.
//
// The classifier is deliberately fail-OPEN: it only labels traffic for
// analytics and fraud scoring — it never instructs the redirect path to
// block, and no input combination can cause a request to be dropped.
// Order of rules:
//
//  1. Empty/blank UA → Known (the single strongest automation signal).
//  2. UA contains a known bot/crawler/library token (case-insensitive)
//     → Known.
//  3. Accept AND Accept-Language both missing → Suspected. Both must be
//     missing; real browsers always send both, but a single absent header
//     is common in benign proxies, so one alone stays Human.
//  4. Otherwise → Human.
//
// The returned reason is a short lowercase explanation ("known bot:
// googlebot", "empty user-agent", "missing accept and accept-language",
// "ok") suitable for structured logging.
func Classify(ua string, hasAcceptHeader, hasAcceptLanguage bool) (BotClass, string) {
	lua := canonicalUA(ua) // lowercased + whitespace-collapsed
	if lua == "" {
		return Known, "empty user-agent"
	}
	for _, tok := range knownBotTokens {
		if strings.Contains(lua, tok) {
			return Known, "known bot: " + tok
		}
	}
	if !hasAcceptHeader && !hasAcceptLanguage {
		return Suspected, "missing accept and accept-language"
	}
	return Human, "ok"
}

// datacenterRanges: major cloud/hosting blocks (v4), dotted-quad string
// comparison ranges [lo, hi] inclusive. Sampled to the largest published
// blocks per provider; paid feeds plug into the same shape later.
var datacenterRanges = [][2]string{
	{"3.0.0.0", "3.255.255.255"},       // AWS
	{"13.32.0.0", "13.47.255.255"},     // AWS CloudFront
	{"18.128.0.0", "18.255.255.255"},   // AWS
	{"52.0.0.0", "52.95.255.255"},      // AWS
	{"54.64.0.0", "54.95.255.255"},     // AWS
	{"34.64.0.0", "34.127.255.255"},    // GCP
	{"35.184.0.0", "35.191.255.255"},   // GCP
	{"20.33.0.0", "20.39.255.255"},     // Azure
	{"104.131.0.0", "104.131.255.255"}, // DigitalOcean
	{"138.197.0.0", "138.197.255.255"}, // DigitalOcean
	{"159.65.0.0", "159.65.255.255"},   // DigitalOcean
	{"5.9.0.0", "5.9.255.255"},         // Hetzner
	{"88.99.0.0", "88.99.255.255"},     // Hetzner
	{"51.75.0.0", "51.75.255.255"},     // OVH
	{"51.83.0.0", "51.83.255.255"},     // OVH
}

// quad converts a dotted-quad v4 string to a comparable uint32. ok=false
// for v6/non-IP strings.
func quad(s string) (uint32, bool) {
	var a, b, c, d uint32
	n, _ := fmt.Sscanf(s, "%d.%d.%d.%d", &a, &b, &c, &d)
	if n != 4 || a > 255 || b > 255 || c > 255 || d > 255 {
		return 0, false
	}
	return a<<24 | b<<16 | c<<8 | d, true
}

// IsDatacenterIP reports whether the IP sits in a known datacenter/hosting
// range (v4; mapped v6 unwrapped). Classification happens at ingest so raw
// IPs never need storage.
func IsDatacenterIP(ip string) bool {
	ip = strings.TrimPrefix(ip, "::ffff:") // 4-in-6 normalization
	if strings.Contains(ip, ":") {
		return false // native v6: no v4 ranges apply (v6 feeds later)
	}
	q, ok := quad(ip)
	if !ok {
		return false
	}
	for _, r := range datacenterRanges {
		lo, ok1 := quad(r[0])
		hi, ok2 := quad(r[1])
		if ok1 && ok2 && q >= lo && q <= hi {
			return true
		}
	}
	return false
}
