// Package postbacks delivers server-to-server conversion notifications to
// affiliate endpoints (the classic affiliate-network integration surface).
// Consumed from the outbox; delivery is at-least-once with retry/backoff.
package postbacks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Emitter polls conversions and delivers postbacks.
type Emitter struct {
	stopOnce sync.Once
	db       *sql.DB
	client   *http.Client
	tick     time.Duration
	workers  int

	// AllowLocalhost bypasses the loopback SSRF guard. TESTS ONLY (httptest
	// servers bind 127.0.0.1); never set in production wiring.
	AllowLocalhost bool

	// BackoffBase shortens the retry ladder in tests. Production default 30s.
	BackoffBase time.Duration

	// TickInterval shortens the poll interval in tests. Production default 5s.
	TickInterval time.Duration

	stop chan struct{}
	done chan struct{}
}

// NewEmitter builds one. Default poll: 5s; timeout 10s.
func NewEmitter(db *sql.DB) *Emitter {
	var e *Emitter
	e = &Emitter{
		db: db,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// SSRF guard is enforced at DIAL time (A12): the hostname is
			// resolved ONCE, every candidate IP is checked, and the
			// connection is made to the validated IP literal — never by
			// hostname (a second resolution is the DNS-rebinding window).
			// The guard closures capture THIS emitter — AllowLocalhost is
			// per-instance, not a process latch (parallel emitters can't
			// flip each other's guard).
			Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return e.dialContext(ctx, network, addr)
			}},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Redirect hops are ALWAYS checked against internal ranges —
				// AllowLocalhost only exempts the INITIAL target (the
				// test-server callback), never a mid-flight pivot to
				// loopback/metadata/link-local. Otherwise a test (or a
				// misconfigured allow) would let a public URL bounce through
				// itself into the metadata service.
				if !e_allowsHost(req.URL.Hostname()) {
					return http.ErrAbortHandler
				}
				return nil
			},
		},
		tick:    5 * time.Second,
		workers: 2,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	return e
}

// dialContext is the SSRF enforcement point (A12). The host is resolved
// exactly once (with the request context) and every returned address must
// be public; the connection then goes to the first validated IP literal
// with the original port, so no later resolution can swap in an internal
// answer (DNS rebinding). net.Dialer.Control re-checks the address the
// socket is actually connecting to — defense in depth for any path that
// reaches the dialer without passing the resolution above.
func (e *Emitter) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	allowLocal := e.AllowLocalhost
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ip := parseAltIPv4(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		if !allowLocal && blockedHostname(host) {
			return nil, fmt.Errorf("postbacks: internal address blocked: %s", host)
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("postbacks: resolve %s: %w", host, err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("postbacks: no addresses for %s", host)
	}
	if !allowLocal {
		for _, ip := range ips {
			if !isPublicIP(ip) {
				return nil, fmt.Errorf("postbacks: internal address blocked: %s", host)
			}
		}
	}
	d := net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		if allowLocal {
			return nil
		}
		h, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(h); ip == nil || !isPublicIP(ip) {
			return fmt.Errorf("postbacks: internal address blocked at connect: %s", h)
		}
		return nil
	}}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// Start launches the poll loop, honoring TickInterval if set.
func (e *Emitter) Start(ctx context.Context) {
	if e.TickInterval > 0 {
		e.tick = e.TickInterval
	}
	go e.run(ctx)
}

// Stop terminates and waits.
func (e *Emitter) Stop() {
	e.stopOnce.Do(func() { close(e.stop) })
	<-e.done
}

func (e *Emitter) run(ctx context.Context) {
	defer close(e.done)
	// First pass immediately (fresh commissions shouldn't wait a full tick);
	// same pattern as the outbox dispatcher.
	if err := e.deliverPending(ctx); err != nil {
		log.Printf("postbacks: %v", err)
	}
	t := time.NewTicker(e.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stop:
			return
		case <-t.C:
			if err := e.deliverPending(ctx); err != nil {
				log.Printf("postbacks: %v", err)
			}
		}
	}
}

// postbackConf is the affiliate's delivery configuration.
type postbackConf struct {
	url string
	// secret, when non-empty, signs every delivery (signed outbound
	// webhooks, FR-29): X-AE-Signature: t=<unix>,v1=hex(hmac_sha256(
	// secret, "<unix>.<path>?<query>")).
	secret string
}

// postbackConfFor loads the affiliate's postback template + signing secret.
func (e *Emitter) postbackConfFor(ctx context.Context, affiliateID int64) (postbackConf, error) {
	var c postbackConf
	err := e.db.QueryRowContext(ctx,
		`SELECT postback_url, postback_secret FROM affiliates WHERE id = ?`, affiliateID).
		Scan(&c.url, &c.secret)
	return c, err
}

// clickParams loads the originating click's traffic-source params for
// {ts:Source:param} macros: dedicated columns (utm_source/subid/placement)
// plus the extra_json.all_params catch-all (FR-2). Nil when the commission
// has no click linkage or the click row is gone.
func (e *Emitter) clickParams(ctx context.Context, clickID string) map[string]string {
	if clickID == "" {
		return nil
	}
	var utm, subid, placement, extra string
	err := e.db.QueryRowContext(ctx,
		`SELECT COALESCE(utm_source,''), COALESCE(subid,''), COALESCE(placement,''), COALESCE(extra_json,'{}')
		 FROM clicks WHERE id = ?`, clickID).
		Scan(&utm, &subid, &placement, &extra)
	if err != nil {
		return nil // no click row (pre-linkage history): macros resolve empty
	}
	var parsed struct {
		AllParams map[string]string `json:"all_params"`
	}
	_ = json.Unmarshal([]byte(extra), &parsed)
	m := parsed.AllParams
	if m == nil {
		m = make(map[string]string, 3)
	}
	// Dedicated columns are addressable under their own names.
	if utm != "" {
		m["utm_source"] = utm
	}
	if subid != "" {
		m["subid"] = subid
	}
	if placement != "" {
		m["placement"] = placement
	}
	return m
}

// deliverPending scans commissions with pending postbacks.
func (e *Emitter) deliverPending(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `
		SELECT c.id, c.affiliate_id, c.amount, c.currency,
		       COALESCE(c.gross_minor, 0),
		       COALESCE(c.click_id, ''), COALESCE(c.order_id, ''),
		       COALESCE(pb.attempts, 0)
		FROM commissions c
		LEFT JOIN postback_state pb ON pb.commission_id = c.id
		WHERE c.status IN ('pending','approved')
		  AND (
		    pb.commission_id IS NULL
		    OR (pb.delivered_at IS NULL AND pb.attempts < 5 AND pb.next_attempt_at <= ?)
		  )
		LIMIT 50`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type job struct {
		commissionID, affiliateID, amount, gross int64
		currency, clickID, orderID               string
		attempts                                 int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.commissionID, &j.affiliateID, &j.amount, &j.currency,
			&j.gross, &j.clickID, &j.orderID, &j.attempts); err != nil {
			return err
		}
		jobs = append(jobs, j)
	}
	_ = rows.Close()

	for _, j := range jobs {
		conf, err := e.postbackConfFor(ctx, j.affiliateID)
		if err != nil || conf.url == "" {
			// No postback configured: mark done so we stop scanning it.
			e.mark(ctx, j.commissionID, j.attempts, true, "no postback url")
			continue
		}
		mc := macroCtx{
			clickID:  j.clickID,
			orderID:  j.orderID,
			amount:   j.amount,
			gross:    j.gross,
			currency: j.currency,
		}
		// Click params are fetched lazily: only templates that reference
		// traffic-source macros pay the lookup cost.
		if strings.Contains(conf.url, "{ts:") {
			mc.params = e.clickParams(ctx, j.clickID)
		}
		target := mc.expand(conf.url)
		if !e.safeTarget(target) {
			e.mark(ctx, j.commissionID, j.attempts, true, "unsafe target")
			continue
		}
		ok := e.fire(ctx, target, conf.secret)
		e.mark(ctx, j.commissionID, j.attempts+1, ok, "")
	}
	return nil
}

// fire GETs the postback; 2xx = success. When secret is non-empty the
// request carries X-AE-Signature (t=<unix>,v1=HMAC-SHA256 over
// "<unix>.<request-uri>") so the receiver can authenticate the delivery.
func (e *Emitter) fire(ctx context.Context, target, secret string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	if secret != "" {
		ts := time.Now().Unix()
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(fmt.Sprintf("%d.", ts)))
		_, _ = mac.Write([]byte(req.URL.RequestURI()))
		req.Header.Set("X-AE-Signature",
			fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// blockedHostname reports names that always point inside the deployment
// (loopback alias, cloud metadata services), whatever a resolver answers.
func blockedHostname(host string) bool {
	switch strings.TrimSuffix(strings.ToLower(host), ".") {
	case "", "localhost", "metadata.google.internal", "instance-data":
		return true
	}
	return false
}

// e_allowsHost is the cheap pre-filter (safeTarget, redirect hops): the
// host must be a public literal or a name whose every address is public.
// It FAILS CLOSED — a name that does not resolve (or resolves to nothing)
// is refused. The old fallthrough ("unresolved yet, dial-time lookup
// re-checks") handed the decision to a second, attacker-influenced
// resolution. Dial-time validation (dialContext) stays the enforcement.
func e_allowsHost(host string) bool {
	if blockedHostname(host) {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPublicIP(ip)
	}
	// ParseIP rejects non-dotted forms (0177.0.0.1 etc.); normalize the
	// classic alternate encodings explicitly.
	if ip := parseAltIPv4(host); ip != nil {
		return isPublicIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return false
		}
	}
	return true
}

// parseAltIPv4 resolves decimal/hex/octal whole-IP encodings (2130706433,
// 0x7f000001, 0177.0.0.1) the way a resolver would.
func parseAltIPv4(host string) net.IP {
	// Whole-number forms.
	if n, err := strconv.ParseUint(host, 0, 64); err == nil && n <= 0xFFFFFFFF {
		return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)) // #nosec G115 -- n <= 0xFFFFFFFF guard makes every byte() truncation exact
	}
	// Dotted forms with hex/octal components.
	if strings.Contains(host, ".") {
		parts := strings.Split(host, ".")
		if len(parts) == 4 {
			ok := true
			var b [4]byte
			for i, p := range parts {
				v, err := strconv.ParseUint(p, 0, 32)
				if err != nil || v > 0xFF {
					ok = false
					break
				}
				b[i] = byte(v)
			}
			if ok {
				return net.IPv4(b[0], b[1], b[2], b[3])
			}
		}
	}
	return nil
}

// blockedV4Nets are special-purpose IPv4 blocks net.IP's predicates do not
// cover but that must never be a postback target: "this network",
// carrier-grade NAT shared space (often reaches provider-internal
// services), IETF protocol assignments, benchmarking, and the reserved
// class-E range (incl. limited broadcast).
var blockedV4Nets = mustCIDRs(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"198.18.0.0/15",
	"240.0.0.0/4",
)

// IPv6 prefixes that embed (or translate to) an IPv4 destination. The
// embedded v4 is what the packet ultimately reaches, so it is judged by
// the v4 rules: a NAT64 or 6to4 spelling of 127.0.0.1 is still loopback.
var (
	nat64WellKnown = mustCIDRs("64:ff9b::/96")[0]   // RFC 6052: v4 in the low 32 bits
	nat64LocalUse  = mustCIDRs("64:ff9b:1::/48")[0] // RFC 8215: site-local translation
	sixToFour      = mustCIDRs("2002::/16")[0]      // RFC 3056: v4 in bits 16..47
	v4Compatible   = mustCIDRs("::/96")[0]          // deprecated ::a.b.c.d form
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("postbacks: bad CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// isPublicIP reports whether ip is a globally routable destination.
// Loopback, RFC1918, ULA (fc00::/7 via IsPrivate), link-local, multicast,
// unspecified, the special-purpose v4 blocks above, and v6 forms that
// embed a non-public v4 are all refused.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return isPublicV4(v4)
	}
	v6 := ip.To16()
	if v6 == nil {
		return false
	}
	if v6.IsLoopback() || v6.IsPrivate() || v6.IsLinkLocalUnicast() ||
		v6.IsUnspecified() || v6.IsMulticast() {
		return false
	}
	switch {
	case nat64WellKnown.Contains(v6):
		return isPublicV4(net.IPv4(v6[12], v6[13], v6[14], v6[15]).To4())
	case nat64LocalUse.Contains(v6):
		// The embedded v4's offset depends on the operator's prefix
		// length and the translator is site-local by definition: never a
		// legitimate public postback target.
		return false
	case sixToFour.Contains(v6):
		return isPublicV4(net.IPv4(v6[2], v6[3], v6[4], v6[5]).To4())
	case v4Compatible.Contains(v6):
		return isPublicV4(net.IPv4(v6[12], v6[13], v6[14], v6[15]).To4())
	}
	return true
}

func isPublicV4(v4 net.IP) bool {
	if v4 == nil {
		return false
	}
	if v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() ||
		v4.IsUnspecified() || v4.IsMulticast() {
		return false
	}
	for _, n := range blockedV4Nets {
		if n.Contains(v4) {
			return false
		}
	}
	return true
}

// Client exposes the transport for the SSRF regression suite.
func (e *Emitter) Client() *http.Client { return e.client }

// IsSafeTargetForTest exposes the pre-filter for the SSRF regression suite.
func (e *Emitter) IsSafeTargetForTest(raw string) bool { return e.safeTarget(raw) }

// FireForTest performs one delivery attempt synchronously and reports the
// transport error (dial/redirect refusals included).
func (e *Emitter) FireForTest(ctx context.Context, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := e.Client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// safeTarget blocks internal addresses (SSRF guard — threat model A12).
// Kept as the cheap pre-filter; dial-time validation is the enforcement.
func (e *Emitter) safeTarget(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return e.AllowLocalhost || e_allowsHost(u.Hostname())
}

// macroCtx carries every value a postback template can reference. The PRD
// macro set (FR-7): {click_id}, {order_id}, {value}/{payout} (commission
// amount fallback), {ts:Source:param} traffic-source substitution.
type macroCtx struct {
	clickID  string
	orderID  string
	amount   int64 // commission minor units
	gross    int64 // order gross minor units (0 = unknown)
	currency string
	params   map[string]string // traffic-source params (click-linked only)
}

// tsMacro matches {ts:Source:param} — source-limited traffic param refs.
var tsMacro = regexp.MustCompile(`\{ts:([^:{}]+):([^:{}]+)\}`)

// expand substitutes macros with URL-encoding.
func (m macroCtx) expand(tmpl string) string {
	// Traffic-source macros first: {ts:Source:fbclid} -> the fbclid value
	// captured on the originating click. Unknown source/param -> empty
	// (the receiver's own default applies), never the literal macro.
	tmpl = tsMacro.ReplaceAllStringFunc(tmpl, func(match string) string {
		g := tsMacro.FindStringSubmatch(match)
		if len(g) != 3 {
			return ""
		}
		return url.QueryEscape(m.params[strings.ToLower(g[2])])
	})
	r := strings.NewReplacer(
		"{click_id}", url.QueryEscape(m.clickID),
		"{order_id}", url.QueryEscape(m.orderID),
		"{commission_cents}", fmt.Sprint(m.amount),
		"{value}", fmt.Sprint(m.gross),
		"{payout}", fmt.Sprint(m.amount),
		"{currency}", m.currency,
	)
	return r.Replace(tmpl)
}

// mark records delivery state; failure schedules exponential backoff.
// Write failures are logged, not returned: mark runs on the sweep path
// after the delivery attempt — a lost mark costs one redundant retry,
// never a false "delivered".
func (e *Emitter) mark(ctx context.Context, commissionID int64, attempts int, ok bool, note string) {
	now := time.Now().UTC()
	if ok {
		if _, err := e.db.ExecContext(ctx, `
			INSERT INTO postback_state (commission_id, attempts, delivered_at, note)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(commission_id) DO UPDATE SET
			  delivered_at = excluded.delivered_at, attempts = excluded.attempts, note = excluded.note`,
			commissionID, attempts, now.Format(time.RFC3339), note); err != nil {
			log.Printf("postbacks: mark delivered commission=%d: %v", commissionID, err)
		}
		return
	}
	next := now.Add(backoff(attempts, e.BackoffBase))
	if _, err := e.db.ExecContext(ctx, `
		INSERT INTO postback_state (commission_id, attempts, next_attempt_at, note)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(commission_id) DO UPDATE SET
		  attempts = excluded.attempts, next_attempt_at = excluded.next_attempt_at`,
		commissionID, attempts, next.Format(time.RFC3339), note); err != nil {
		log.Printf("postbacks: mark retry commission=%d: %v", commissionID, err)
	}
}

// backoff ladder: base, 3x, 9x, ... capped at 2h (production base 30s).
func backoff(attempts int, base time.Duration) time.Duration {
	if base <= 0 {
		base = 30 * time.Second
	}
	d := base
	for i := 1; i < attempts && i < 5; i++ {
		d *= 3
		if d > 2*time.Hour {
			d = 2 * time.Hour
		}
	}
	return d
}
