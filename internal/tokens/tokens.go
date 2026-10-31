// Package tokens implements HMAC-signed opaque attribution tokens.
//
// The attribution cookie carries ONLY {click_id, affiliate_id, exp}, signed
// with HMAC-SHA256: clients can read nothing else, forge nothing, and any
// byte flipped anywhere in the token invalidates it.
//
// Token format: base64url(payloadJSON) + "." + base64url(HMAC-SHA256(payload)).
// Stripe's client_reference_id carries the same token as "at1_" + payload +
// "_" + sig (no "." allowed there; see ClientReferenceID).
//
// Provenance: clean-room implementation for this project's security threat
// model (research/final/security-threat-model.md); standard-library
// constructs only, no third-party JWT/JWS code copied.
package tokens

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MinSecretLen is the minimum HMAC key size accepted by New. SHA-256 keys
// below 32 bytes reduce the effective MAC strength and are rejected.
const MinSecretLen = 32

// Sentinel parse errors. Tampering, truncation, bad base64, bad JSON, and
// signature mismatch all report ErrMalformed; expiry reports ErrExpired.
var (
	ErrMalformed = errors.New("tokens: malformed token")
	ErrExpired   = errors.New("tokens: token expired")
)

// Payload is the signed token body — the only data the cookie exposes.
type Payload struct {
	ClickID     string `json:"click_id"`
	AffiliateID int64  `json:"affiliate_id"`
	Exp         int64  `json:"exp"` // unix seconds, inclusive cutoff
}

// Signer signs and verifies attribution tokens under one HMAC secret.
type Signer struct {
	secret []byte
	now    func() time.Time // clock, swappable in tests
}

// New returns a Signer for the given HMAC secret. The secret is copied and
// must be at least MinSecretLen bytes; shorter keys are an error.
func New(secret []byte) (*Signer, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("tokens: secret must be >= %d bytes, got %d", MinSecretLen, len(secret))
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &Signer{secret: cp, now: time.Now}, nil
}

// NewRandomSecret returns n cryptographically random bytes (crypto/rand),
// suitable as a Signer secret. n must be >= MinSecretLen.
func NewRandomSecret(n int) ([]byte, error) {
	if n < MinSecretLen {
		return nil, fmt.Errorf("tokens: random secret must be >= %d bytes, got %d", MinSecretLen, n)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("tokens: read random secret: %w", err)
	}
	return b, nil
}

// Sign returns base64url(payload) + "." + base64url(HMAC-SHA256(payload)).
// The payload JSON is compact (no whitespace). ttl <= 0 is permitted and
// produces a token Parse rejects with ErrExpired (caller bug surfaced at
// verification time rather than silently accepted).
func (s *Signer) Sign(clickID string, affiliateID int64, ttl time.Duration) (string, error) {
	p := Payload{
		ClickID:     clickID,
		AffiliateID: affiliateID,
		Exp:         s.now().Add(ttl).Unix(),
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("tokens: marshal payload: %w", err)
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Parse verifies the token and returns its payload.
//
// Verification order: decode both segments (malformed input is untrusted, so
// failure here is ErrMalformed), reject non-canonical base64 encodings (a
// decoded segment must re-encode to the exact input, so each payload has
// exactly ONE valid token string — no malleable trailing padding bits),
// then compare the FULL recomputed HMAC with hmac.Equal — timing-safe, no
// early exit — and only then unmarshal the payload and check exp
// (ErrExpired). Payload semantics are never consulted before the MAC check
// passes.
func (s *Signer) Parse(token string) (Payload, error) {
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return Payload{}, ErrMalformed
	}
	payloadB64, sigB64 := token[:dot], token[dot+1:]
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return Payload{}, ErrMalformed
	}
	if base64.RawURLEncoding.EncodeToString(payload) != payloadB64 {
		return Payload{}, ErrMalformed // non-canonical encoding
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return Payload{}, ErrMalformed
	}
	if base64.RawURLEncoding.EncodeToString(sig) != sigB64 {
		return Payload{}, ErrMalformed // non-canonical encoding
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Payload{}, ErrMalformed
	}
	var p Payload
	if err := json.Unmarshal(payload, &p); err != nil {
		return Payload{}, ErrMalformed
	}
	if s.now().Unix() >= p.Exp {
		return Payload{}, ErrExpired
	}
	return p, nil
}

// ClientReferencePrefix marks a token carried in Stripe's
// client_reference_id. Versioned so the encoding can change later.
const ClientReferencePrefix = "at1_"

const (
	// sigB64Len is the raw base64url length of an HMAC-SHA256 signature.
	sigB64Len = 43
	// maxClientReferenceLen is Stripe's client_reference_id limit.
	maxClientReferenceLen = 200
)

// ClientReferenceID encodes a token for Stripe's client_reference_id,
// which allows only alphanumerics, "-" and "_": "at1_" + payload + "_" +
// sig. ok is false when token is not token-shaped or the result would
// pass Stripe's 200-char limit. t.js mirrors this encoding.
func ClientReferenceID(token string) (string, bool) {
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 || len(token)-dot-1 != sigB64Len {
		return "", false
	}
	ref := ClientReferencePrefix + token[:dot] + "_" + token[dot+1:]
	if len(ref) > maxClientReferenceLen || !isRefChars(ref) {
		return "", false
	}
	return ref, true
}

// FromClientReferenceID reverses ClientReferenceID. The payload is
// base64url and may hold "_" itself, so the split is by position: the
// signature is always the last 43 chars. ok is false for anything else (a
// merchant's own ids share the field). The token is NOT verified here;
// Signer.Parse does that.
func FromClientReferenceID(ref string) (string, bool) {
	rest, found := strings.CutPrefix(ref, ClientReferencePrefix)
	if !found || len(ref) > maxClientReferenceLen || !isRefChars(ref) {
		return "", false
	}
	cut := len(rest) - sigB64Len - 1
	if cut < 1 || rest[cut] != '_' {
		return "", false
	}
	return rest[:cut] + "." + rest[cut+1:], true
}

// isRefChars reports whether s holds only [A-Za-z0-9_-].
func isRefChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// WriteCookie sets the attribution token as a cookie: HttpOnly (no JS
// access), SameSite=Lax (sent on top-level navigations, the conversion
// path, but not cross-site POSTs), Path=/, Secure per flag. A non-empty
// domain widens the cookie to subdomains; empty keeps it host-only.
func WriteCookie(w http.ResponseWriter, name, token string, ttl time.Duration, domain string, secure bool) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure is the dynamic `secure` arg (config-driven, auto-on for https); HttpOnly + SameSite=Lax are always set
		Name:     name,
		Value:    token,
		Path:     "/",
		Domain:   domain,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ReadCookie returns the named cookie's value, or "" if absent/unreadable.
func ReadCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
