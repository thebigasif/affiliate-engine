// Portal auth: affiliate self-serve application + admin approval + session
// login. Sessions are opaque tokens stored hashed; the portal reads
// (earnings/ledger) accept session auth in addition to API keys.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

const (
	portalSessionTTL    = 30 * 24 * time.Hour
	portalSessionCookie = "ae_portal_session"
)

// --- helpers ---

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newPortalToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "aps_" + hex.EncodeToString(b), nil
}

// sessionCookieName is the portal session cookie's name: __Host- over
// HTTPS, so no sibling subdomain (another workspace on the cloud's
// domain) can plant or overwrite it. Plain HTTP (local self-host) can't
// use the prefix. Sessions under the old name read as signed out.
func (a *api) sessionCookieName(r *http.Request) string {
	if a.cookieSecure(r) {
		return "__Host-" + portalSessionCookie
	}
	return portalSessionCookie
}

// portalToken extracts a session token: Bearer header first, then the
// session cookie.
func (a *api) portalToken(r *http.Request) string {
	if tok := bearerToken(r); tok != "" {
		return tok
	}
	if c, err := r.Cookie(a.sessionCookieName(r)); err == nil {
		return c.Value
	}
	return ""
}

// sameOrigin refuses a browser POST from another origin. The portal's
// form posts (tax form upload, unsubscribe, logout) carry the session
// cookie, and SameSite=Lax still sends it from a sibling workspace, which
// is the same site: only same-origin passes. Requests without the
// browser headers (API clients, mail providers' one-click unsubscribe)
// carry no cookie a browser attached for someone, and pass.
func (a *api) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if crossOrigin(r) {
			apiError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next(w, r)
	}
}

func crossOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "":
		// Older browsers: fall back to Origin.
	default: // same-site, cross-site
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err != nil || !strings.EqualFold(u.Host, r.Host)
}

// PortalAuth resolves a session token to an affiliate id (0 = no auth).
// Only ACTIVE affiliates authenticate: freezing (fraud enforcement) or
// removing (erasure) an affiliate cuts off every live session at once.
func (a *api) PortalAuth(ctx context.Context, r *http.Request) int64 {
	tok := a.portalToken(r)
	if tok == "" {
		return 0
	}
	var aff int64
	err := a.db.QueryRowContext(ctx, `
		SELECT s.affiliate_id FROM affiliate_sessions s
		JOIN affiliates af ON af.id = s.affiliate_id
		WHERE s.token_hash = ? AND s.revoked_at IS NULL AND s.expires_at > ?
		  AND af.status = 'active'`,
		hashToken(tok), time.Now().UTC().Format(time.RFC3339)).Scan(&aff)
	if err != nil {
		return 0
	}
	return aff
}

// clientHost strips the ephemeral port from RemoteAddr — the rate limiter
// must key on the peer IP, not ip:port (a fresh bucket per connection
// would make the limit decorative).
func clientHost(remote string) string {
	if h, _, err := net.SplitHostPort(remote); err == nil {
		return h
	}
	return remote
}

// public wraps rate-limited public routes (no auth required).
func (a *api) public(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r, a.deps.TrustedProxies)
		if !a.rl.allow("pub:" + ip) {
			apiError(w, http.StatusTooManyRequests, "rate limit")
			return
		}
		next(w, r)
	}
}

// cookieSecure decides the Secure attribute per request: always on for a
// TLS request (direct, or via a trusted proxy's X-Forwarded-Proto), and
// forced on by config (AE_COOKIE_SECURE=1 or an https AE_PUBLIC_URL).
func (a *api) cookieSecure(r *http.Request) bool {
	return a.deps.SecureCookies || SecureCookiesFromEnv() || requestIsHTTPS(r, a.deps.TrustedProxies)
}

// handlePortalLoginQ extracts the grant from the JSON body and delegates.
// The grant is NEVER read from the query string: URLs land in access logs,
// proxy logs and browser history, and a login grant is a bearer secret.
func (a *api) handlePortalLoginQ(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Grant string `json:"grant"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req)
	a.handlePortalLogin(w, r, strings.TrimSpace(req.Grant))
}

// handlePortalLogout: POST /portal/logout — deletes the caller's session
// (Bearer or cookie) and clears the cookie. Idempotent: no session is not
// an error.
func (a *api) handlePortalLogout(w http.ResponseWriter, r *http.Request) {
	if tok := a.portalToken(r); tok != "" {
		if _, err := a.db.ExecContext(r.Context(),
			`DELETE FROM affiliate_sessions WHERE token_hash = ?`, hashToken(tok)); err != nil {
			apiError(w, http.StatusInternalServerError, "logout failed")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- expiring cookie; Secure from cookieSecure(), HttpOnly + SameSite=Lax always set
		Name: a.sessionCookieName(r), Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cookieSecure(r),
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

// --- public routes (no auth) ---

// handlePortalApply: POST /portal/apply {email, name, note}
func (a *api) handlePortalApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(req.Email, "@") || len(req.Email) > 254 {
		apiError(w, http.StatusBadRequest, "valid email required")
		return
	}
	// Duplicate application or existing affiliate: same neutral answer
	// (no account-enumeration oracle).
	_, err := a.db.ExecContext(r.Context(), `
		INSERT INTO affiliate_applications (email, name, note) VALUES (?, ?, ?)`,
		req.Email, trimTo(req.Name, 120), trimTo(req.Note, 500))
	if err != nil {
		if db.IsUniqueViolation(err) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "received"})
			return
		}
		apiError(w, http.StatusInternalServerError, "apply failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "received"})
}

// handlePortalLogin: POST /portal/login {grant} -> session issued ONLY if
// the affiliate exists+active AND an admin has issued a login grant.
// (Passwordless bootstrap: admin approves application -> grants login ->
// affiliate gets the token via email. No password storage at all.)
func (a *api) handlePortalLogin(w http.ResponseWriter, r *http.Request, grant string) {
	// grant is the one-time login token an admin generated.
	if grant == "" {
		apiError(w, http.StatusBadRequest, "grant required")
		return
	}
	var affiliateID int64
	var usedAt sql.NullString
	err := a.db.QueryRowContext(r.Context(), `
		SELECT g.affiliate_id, g.used_at FROM portal_login_grants g
		JOIN affiliates af ON af.id = g.affiliate_id
		WHERE g.token_hash = ? AND g.expires_at > ? AND af.status = 'active'`,
		hashToken(grant), time.Now().UTC().Format(time.RFC3339)).Scan(&affiliateID, &usedAt)
	if err == sql.ErrNoRows {
		apiError(w, http.StatusUnauthorized, "invalid or expired grant")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "grant lookup failed")
		return
	}
	if usedAt.Valid {
		apiError(w, http.StatusUnauthorized, "grant already used")
		return
	}

	// Mark grant used + issue session in one tx.
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `
		UPDATE portal_login_grants SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), hashToken(grant))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "grant consume failed")
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Grant already consumed (or never existed): the row-level guard
		// makes concurrent double-spend impossible — exactly one UPDATE wins.
		apiError(w, http.StatusUnauthorized, "invalid or consumed grant")
		return
	}
	tok, err := newPortalToken()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "token gen failed")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO affiliate_sessions (token_hash, affiliate_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(tok), affiliateID, time.Now().UTC().Add(portalSessionTTL).Format(time.RFC3339)); err != nil {
		apiError(w, http.StatusInternalServerError, "session insert failed")
		return
	}
	if err := tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure comes from cookieSecure() (TLS / trusted X-Forwarded-Proto / AE_COOKIE_SECURE / https AE_PUBLIC_URL); HttpOnly + SameSite=Lax always set
		Name: a.sessionCookieName(r), Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cookieSecure(r),
		MaxAge: int(portalSessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"affiliate_id": affiliateID, "session": tok})
}

// handleGamifyProfile: GET /affiliate/profile (session or affiliate key).
func (a *api) handleGamifyProfile(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	if a.deps.Gamify == nil {
		apiError(w, http.StatusServiceUnavailable, "gamification not wired")
		return
	}
	id := idn.AffiliateID
	if id == 0 { // admin may query ?affiliate_id=
		if q := r.URL.Query().Get("affiliate_id"); q != "" {
			var err error
			id, err = parseInt64(q)
			if err != nil || id <= 0 {
				apiError(w, http.StatusBadRequest, "bad affiliate_id")
				return
			}
		}
	}
	if id == 0 {
		apiError(w, http.StatusBadRequest, "affiliate scope required")
		return
	}
	p, err := a.deps.Gamify.Profile(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "profile failed")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleGamifyLeaderboard: GET /leaderboard (public-safe: names + xp only).
func (a *api) handleGamifyLeaderboard(w http.ResponseWriter, r *http.Request) {
	if a.deps.Gamify == nil {
		apiError(w, http.StatusServiceUnavailable, "gamification not wired")
		return
	}
	lb, err := a.deps.Gamify.Leaderboard(r.Context(), 20)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "leaderboard failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"leaderboard": lb})
}

func parseInt64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// --- admin routes ---

// handlePortalQueue: GET /admin/portal/applications
func (a *api) handlePortalQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, email, name, note, status, created_at
		FROM affiliate_applications ORDER BY id DESC LIMIT 200`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	type app struct {
		ID        int64  `json:"id"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		Note      string `json:"note"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	out := []app{}
	for rows.Next() {
		var x app
		if err := rows.Scan(&x.ID, &x.Email, &x.Name, &x.Note, &x.Status, &x.CreatedAt); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": out})
}

// handlePortalReview: POST /admin/portal/review
// {"application_id": N, "decision": "approve" | "reject", "reason": "..."}
// reject => with email on, the applicant gets a short, polite note,
// including the reason when one is given.
// approve => affiliate row created (if new), a primary referral code
// minted if it has none (autocode.go) + one-time login grant minted
// (shown once in the response for the admin to deliver).
func (a *api) handlePortalReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApplicationID int64  `json:"application_id"`
		Decision      string `json:"decision"`
		Reason        string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.ApplicationID <= 0 || (req.Decision != "approve" && req.Decision != "reject") {
		apiError(w, http.StatusBadRequest, "application_id + decision (approve|reject) required")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if utf8.RuneCountInString(req.Reason) > 1000 {
		apiError(w, http.StatusBadRequest, "Keep the reason under 1,000 characters.")
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback()

	var email, name string
	var status string
	err = tx.QueryRowContext(r.Context(), `
		SELECT email, name, status FROM affiliate_applications WHERE id = ?`,
		req.ApplicationID).Scan(&email, &name, &status)
	if err == sql.ErrNoRows {
		apiError(w, http.StatusNotFound, "no such application")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if status != "pending" {
		apiError(w, http.StatusConflict, "already reviewed: "+status)
		return
	}

	if req.Decision == "reject" {
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE affiliate_applications SET status='rejected', reviewed_at=?
			WHERE id = ? AND status='pending'`,
			time.Now().UTC().Format(time.RFC3339), req.ApplicationID); err != nil {
			apiError(w, http.StatusInternalServerError, "reject failed")
			return
		}
		if a.deps.MailOn {
			if err := partnermail.EnqueueRejected(r.Context(), tx, req.ApplicationID, req.Reason); err != nil {
				apiError(w, http.StatusInternalServerError, "email queue failed")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			apiError(w, http.StatusInternalServerError, "commit failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"application_id": req.ApplicationID, "decision": "rejected", "emailed": a.deps.MailOn})
		return
	}

	// approve: create affiliate if not exists.
	var affiliateID int64
	err = tx.QueryRowContext(r.Context(), `
		SELECT id FROM affiliates WHERE email = ?`, email).Scan(&affiliateID)
	if err == sql.ErrNoRows {
		if err := tx.QueryRowContext(r.Context(), `
			INSERT INTO affiliates (email, name) VALUES (?, ?) RETURNING id`,
			email, name).Scan(&affiliateID); err != nil {
			apiError(w, http.StatusInternalServerError, "affiliate create failed")
			return
		}
	} else if err != nil {
		apiError(w, http.StatusInternalServerError, "affiliate lookup failed")
		return
	}

	// First referral code (/r/{code}), name-based. Admins can change it
	// through /admin/codes; affiliates cannot.
	refCode, err := ensurePrimaryCode(r.Context(), tx, affiliateID, name, email)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "referral code create failed")
		return
	}

	// One-time login grant (24h).
	grant, err := newPortalToken()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "grant gen failed")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO portal_login_grants (token_hash, affiliate_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(grant), affiliateID,
		time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339)); err != nil {
		apiError(w, http.StatusInternalServerError, "grant insert failed")
		return
	}
	// With email on, the affiliate gets their own link too (minted when the
	// email goes out; the grant above stays for the admin to copy).
	if a.deps.MailOn {
		if err := partnermail.EnqueueLoginLink(r.Context(), tx, affiliateID, partnermail.ReasonApproved); err != nil {
			apiError(w, http.StatusInternalServerError, "email queue failed")
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE affiliate_applications SET status='approved', reviewed_at=?
		WHERE id = ? AND status='pending'`,
		time.Now().UTC().Format(time.RFC3339), req.ApplicationID); err != nil {
		apiError(w, http.StatusInternalServerError, "app update failed")
		return
	}
	if err := tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"application_id": req.ApplicationID, "decision": "approved",
		"affiliate_id": affiliateID, "referral_code": refCode,
		"login_grant": grant, // shown once
		"emailed":     a.deps.MailOn,
	})
}

// handlePortalLoginLink: POST /portal/login-link {email}. An active
// affiliate gets a one-time login link (1 hour) by email. The answer is
// the same whether or not the email belongs to an affiliate (no account
// enumeration), and one address gets at most one email per 5 minutes.
func (a *api) handlePortalLoginLink(w http.ResponseWriter, r *http.Request) {
	if !a.deps.MailOn {
		apiError(w, http.StatusServiceUnavailable, "this program doesn't send emails; ask the program owner for a login link")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		apiError(w, http.StatusBadRequest, "valid email required")
		return
	}
	sent := map[string]any{"status": "sent"}
	if !a.loginLinks.allow(email, time.Now()) {
		writeJSON(w, http.StatusOK, sent)
		return
	}
	var affID int64
	err := a.db.QueryRowContext(r.Context(), `
		SELECT id FROM affiliates WHERE LOWER(email) = ? AND status = 'active'`, email).Scan(&affID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, sent)
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if err := partnermail.EnqueueLoginLink(r.Context(), a.db, affID, partnermail.ReasonRequested); err != nil {
		apiError(w, http.StatusInternalServerError, "email queue failed")
		return
	}
	writeJSON(w, http.StatusOK, sent)
}

// loginLinkLimiter allows one login-link email per address per window.
type loginLinkLimiter struct {
	mu     sync.Mutex
	window time.Duration
	last   map[string]time.Time
}

func newLoginLinkLimiter(window time.Duration) *loginLinkLimiter {
	return &loginLinkLimiter{window: window, last: map[string]time.Time{}}
}

func (l *loginLinkLimiter) allow(email string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.last) > 10000 { // bounded memory: drop expired entries
		for k, t := range l.last {
			if now.Sub(t) >= l.window {
				delete(l.last, k)
			}
		}
	}
	if t, ok := l.last[email]; ok && now.Sub(t) < l.window {
		return false
	}
	l.last[email] = now
	return true
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
