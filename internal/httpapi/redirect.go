// Package httpapi exposes the public HTTP surface: the /r/{code} redirect
// endpoint (click capture + token mint) and (later) webhooks and portals.
package httpapi

import (
	"database/sql"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/fingerprint"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// Click field caps: attacker-controlled strings go straight into clicks
// rows (and later postback macros), so each is bounded before storage.
const (
	maxRefererLen = 1024
	maxLandingLen = 1024
	maxDimLen     = 256 // utm_source / subid / placement

	defaultRedirectRatePerMin = 120
)

// Config for the redirect handler.
type Config struct {
	Signer       *tokens.Signer
	CookieName   string // default "ae_attribution"
	CookieDomain string
	// CookieSecure forces Secure; TLS requests get it regardless.
	CookieSecure   bool
	AttributionTTL time.Duration // default 30 days
	// AllowedDestSuffixes: destination host must equal or end with one of
	// these (open-redirect allowlist, Refferq's proven pattern).
	AllowedDestSuffixes []string
	FallbackURL         string
	// IPMode anonymizes the client IP at write time (NFR-7 privacy
	// option; the raw IP is never stored — this bounds the entropy that
	// enters the identity hashes). Default: full precision.
	IPMode privacy.IPMode
	// TrustedProxies: only peers inside these CIDRs may supply
	// X-Forwarded-For / X-Forwarded-Proto and CDN geo headers. Empty =
	// trust no proxy (RemoteAddr is the client).
	TrustedProxies []netip.Prefix
	// RateLimitPerMin caps /r/ hits per client IP per minute (0 = default
	// 120; negative disables).
	RateLimitPerMin int
}

// RedirectHandler captures clicks and redirects.
type RedirectHandler struct {
	db        *sql.DB
	cfg       Config
	committer *clicks.Committer
	rl        *rateLimiter // nil = unlimited
}

// NewRedirectHandler builds the handler. The committer is owned by main.
func NewRedirectHandler(db *sql.DB, cfg Config, committer *clicks.Committer) *RedirectHandler {
	if cfg.CookieName == "" {
		cfg.CookieName = "ae_attribution"
	}
	if cfg.AttributionTTL == 0 {
		cfg.AttributionTTL = 30 * 24 * time.Hour
	}
	if cfg.FallbackURL == "" {
		cfg.FallbackURL = "/"
	}
	h := &RedirectHandler{db: db, cfg: cfg, committer: committer}
	switch {
	case cfg.RateLimitPerMin == 0:
		h.rl = newRateLimiter(defaultRedirectRatePerMin)
	case cfg.RateLimitPerMin > 0:
		h.rl = newRateLimiter(cfg.RateLimitPerMin)
	}
	return h
}

// ServeHTTP handles GET /r/{code}?dest=...&utm_*=&subid=&placement=
func (h *RedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := strings.TrimPrefix(r.URL.Path, "/r/")
	code = strings.Trim(code, "/")
	if code == "" || strings.Contains(code, "/") {
		http.NotFound(w, r)
		return
	}
	clientIP := ClientIP(r, h.cfg.TrustedProxies)
	// Per-IP flood guard, before any DB work: a click farm hammering /r/
	// must not be able to fill the clicks table or the committer queue.
	if h.rl != nil && !h.rl.allow("r:"+clientIP) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	ctx := r.Context()

	// Resolve affiliate by referral code: slug first (imported incumbent links
	// keep working — FR-32 slug preservation), numeric id as fallback.
	// Campaign comes from the CODE (per-program links), falling back to the
	// single-program era default of 1 — with the program's attribution
	// window resolved alongside (per-program config, was hardcoded 30d).
	var affiliateID, campaignID int64
	var status string
	var windowDays int
	refCode := strings.ToLower(code)
	err := h.db.QueryRowContext(ctx,
		`SELECT a.id, COALESCE(c.campaign_id, 1), a.status, COALESCE(cp.attribution_window_days, 30)
		 FROM affiliates a
		 JOIN affiliate_codes c ON c.affiliate_id = a.id
		 LEFT JOIN campaigns cp ON cp.id = COALESCE(c.campaign_id, 1)
		 WHERE c.code = ?`, refCode).
		Scan(&affiliateID, &campaignID, &status, &windowDays)
	if err == sql.ErrNoRows {
		refCode = "" // not a code: the click is not recorded against one
		// Fallback: numeric affiliate id (legacy links).
		if id := codeNumericID(code); id > 0 {
			err = h.db.QueryRowContext(ctx,
				`SELECT a.id, 1, a.status, COALESCE(cp.attribution_window_days, 30)
				 FROM affiliates a
				 LEFT JOIN campaigns cp ON cp.id = 1
				 WHERE a.id = ?`, id).
				Scan(&affiliateID, &campaignID, &status, &windowDays)
		}
	}
	if err != nil || status != "active" {
		// Unknown/inactive: still redirect (safe), no tracking.
		http.Redirect(w, r, h.safeDest(r), http.StatusFound) // #nosec G710 -- destination is allowlist-validated by safeDest (the guard this taint analysis cannot see through)
		return
	}

	// Bot classification (fail-open: bots redirect, get stored, excluded).
	ua := r.Header.Get("User-Agent")
	bc, _ := fingerprint.Classify(ua,
		r.Header.Get("Accept") != "", r.Header.Get("Accept-Language") != "")
	botClass := int(bc)

	// Identity hashes (IP-method identity over canonical IP; UA-method over UA).
	// NFR-7: with AE_IP_MODE=trunc24 the IP is truncated to /24 (v4) or
	// /48 (v6) BEFORE hashing — write-time anonymization; unique counting
	// and velocity rules degrade to subnet granularity by design.
	ip := privacy.AnonymizeIP(h.cfg.IPMode, clientIP)
	ipHashArr := fingerprint.NewIdentity(fingerprint.ByIP, ip)
	uaHashArr := fingerprint.NewIdentity(fingerprint.ByIPUA, ip, ua)
	ipHash := ipHashArr[:]
	uaHash := uaHashArr[:]
	isDC := fingerprint.IsDatacenterIP(ip)

	// CDN geo headers are only as trustworthy as the peer that set them:
	// a direct client could otherwise pick any country (geo fraud rules).
	country := ""
	if peerTrusted(r, h.cfg.TrustedProxies) {
		country = geoCountry(r)
	}

	// Enqueue the click (async; committed in batches). The attribution row
	// is written inside the same batch transaction (FK-safe). The signed
	// cookie token is self-contained and set immediately — it does not
	// depend on the batch landing.
	// Per-program window (resolved above) overrides the global default:
	// the cookie lifetime and the signed-token expiry must match the
	// program the click belongs to, not a process-wide constant.
	ttl := time.Duration(windowDays) * 24 * time.Hour
	q := r.URL.Query()
	clickID, _, err := h.committer.Enqueue(affiliateID, campaignID, clicks.Click{
		IPHash:                 ipHash,
		UAHash:                 uaHash,
		Referer:                truncateUTF8(r.Header.Get("Referer"), maxRefererLen),
		LandingPath:            truncateUTF8(safeLanding(r), maxLandingLen),
		UTMSource:              truncateUTF8(q.Get("utm_source"), maxDimLen),
		SubID:                  truncateUTF8(q.Get("subid"), maxDimLen),
		Placement:              truncateUTF8(q.Get("placement"), maxDimLen),
		Country:                country,
		BotClass:               botClass,
		IsDC:                   isDC,
		RefCode:                refCode,
		ExtraParams:            extraParams(r),
		AttributionAffiliateID: affiliateID,
		AttributionCampaignID:  campaignID,
		AttributionTTL:         ttl,
	}, time.Now())
	dest, allowed := h.allowedDest(r)
	if err == nil {
		if tok, terr := h.cfg.Signer.Sign(clickID, affiliateID, ttl); terr == nil {
			tokens.WriteCookie(w, h.cfg.CookieName, tok, ttl,
				h.cfg.CookieDomain, h.cfg.CookieSecure || requestIsHTTPS(r, h.cfg.TrustedProxies))
			// The cookie is host-only on the engine's host, which the
			// merchant's site can never read. The token rides along in the
			// URL instead, where t.js picks it up first-party.
			if allowed {
				dest = withQueryParam(dest, "aff_token", tok)
			}
		}
	}

	http.Redirect(w, r, dest, http.StatusFound) // #nosec G710 -- destination is allowlist-validated by allowedDest (the guard this taint analysis cannot see through)
}

// safeDest validates the destination against the allowlist (open-redirect guard).
func (h *RedirectHandler) safeDest(r *http.Request) string {
	dest, _ := h.allowedDest(r)
	return dest
}

// allowedDest returns the requested destination and true when it passes
// the allowlist, else the fallback URL. A bare /r/{code} (no destination at
// all) reports true when the fallback is itself on the allowlist, the
// merchant's own site, so the token rides along there too. A destination
// that fails the allowlist never does: that link is broken or tampered.
func (h *RedirectHandler) allowedDest(r *http.Request) (string, bool) {
	dest := r.URL.Query().Get("dest")
	if dest == "" {
		dest = r.URL.Query().Get("url")
	}
	if dest == "" {
		return h.cfg.FallbackURL, h.destAllowed(h.cfg.FallbackURL)
	}
	if h.destAllowed(dest) {
		return dest, true
	}
	return h.cfg.FallbackURL, false
}

// destAllowed reports whether dest is an http(s) URL on an allowlisted host.
func (h *RedirectHandler) destAllowed(dest string) bool {
	if dest == "" {
		return false
	}
	u, err := url.Parse(dest)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range h.cfg.AllowedDestSuffixes {
		suffix = strings.ToLower(suffix)
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// withQueryParam sets key=value on an already-validated URL string. The
// query is edited as raw text: other params keep their order and exact
// encoding, the fragment stays in place, and any existing key is
// replaced (a stale token from an earlier click must not win).
func withQueryParam(dest, key, value string) string {
	base, frag, hasFrag := strings.Cut(dest, "#")
	path, query, _ := strings.Cut(base, "?")
	var kept []string
	if query != "" {
		for _, pair := range strings.Split(query, "&") {
			k, _, _ := strings.Cut(pair, "=")
			if dk, err := url.QueryUnescape(k); err == nil && dk == key {
				continue
			}
			kept = append(kept, pair)
		}
	}
	kept = append(kept, url.QueryEscape(key)+"="+url.QueryEscape(value))
	out := path + "?" + strings.Join(kept, "&")
	if hasFrag {
		out += "#" + frag
	}
	return out
}

// codeNumericID returns the numeric affiliate id for pure-digit codes, or
// -1 (never a valid id) for slug codes.
func codeNumericID(s string) int64 {
	if s == "" {
		return -1
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func safeLanding(r *http.Request) string {
	dest := r.URL.Query().Get("dest")
	if dest == "" {
		return "/"
	}
	u, err := url.Parse(dest)
	if err != nil {
		return "/"
	}
	return u.Path
}

// truncateUTF8 caps s at n bytes without splitting a multi-byte rune
// (PostgreSQL rejects invalid UTF-8 in TEXT columns).
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// geoCountry reads the CDN geo header set at the edge (the privacy-first
// capture point: alpha-2 only, no raw location data, mirroring the IP-hash
// discipline). CF-IPCountry is the Cloudflare convention; the aliases cover
// Vercel/Fly/nginx-geo patterns. Callers must only use it for requests
// from a trusted proxy.
func geoCountry(r *http.Request) string {
	for _, h := range []string{"Cf-Ipcountry", "X-Vercel-Ip-Country", "X-Geo-Country", "X-Country-Code"} {
		if v := strings.TrimSpace(r.Header.Get(h)); len(v) == 2 {
			return strings.ToUpper(v)
		}
	}
	return ""
}

// claimedParams are query keys with dedicated clicks columns; everything
// else lands in extra_json.all_params (FR-2 catch-all).
var claimedParams = map[string]bool{
	"dest": true, "url": true,
	"utm_source": true, "subid": true, "placement": true,
}

// extraParams captures every non-claimed redirect query param (the FR-2
// extra_json catch-all — the data source for postback {ts:Source:param}
// macros, FR-7). Values are stored verbatim (URL-decoded by ParseQuery);
// caps: 32 params, 256 bytes per value, 64 per key (defense in depth —
// extra_json is a catch-all, not a dumping ground).
func extraParams(r *http.Request) map[string]string {
	q := r.URL.Query()
	var out map[string]string
	for k, vs := range q {
		if claimedParams[k] || len(out) >= 32 {
			continue
		}
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		v = truncateUTF8(v, 256)
		k = truncateUTF8(k, 64)
		if out == nil {
			out = make(map[string]string, 4)
		}
		out[k] = v
	}
	return out
}
