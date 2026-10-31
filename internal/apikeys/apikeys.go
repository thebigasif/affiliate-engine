// Package apikeys implements scoped, hashed API keys (FR-29).
//
// Keys are shown once at creation and stored only as SHA-256 hashes.
// Format: "ae_" + 32 base62-random chars. Scopes: "admin" or "affiliate:<id>".
// Lookup is by hash (constant-time compare via the unique index equality is
// not possible to time-attack meaningfully; hashes are not secrets derived
// from low-entropy inputs).
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Scope values.
const (
	ScopeAdmin = "admin"
	// ScopeAffiliate builds an affiliate-scoped string.
	ScopeAffiliate = "affiliate"
)

// AffiliateScope returns the scope string for one affiliate.
func AffiliateScope(id int64) string { return fmt.Sprintf("affiliate:%d", id) }

// IsAdminScope reports whether the scope grants admin (all) access.
func IsAdminScope(scope string) bool { return scope == ScopeAdmin }

// ScopeAffiliateID parses "affiliate:<id>"; returns ok=false for admin or malformed.
func ScopeAffiliateID(scope string) (int64, bool) {
	rest, ok := strings.CutPrefix(scope, ScopeAffiliate+":")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Generate returns a new full key (shown once) and its stored hash.
func Generate() (key, hash, prefix string, err error) {
	b := make([]byte, 32)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(62))
		if err != nil {
			return "", "", "", fmt.Errorf("apikeys: rand: %w", err)
		}
		b[i] = base62[n.Int64()]
	}
	key = "ae_" + string(b)
	return key, HashKey(key), key[:10], nil
}

// HashKey returns the hex SHA-256 of the full key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Store creates and persists a key. Returns the plaintext ONCE.
func Store(ctx context.Context, db *sql.DB, name, scope string) (key string, err error) {
	k, hash, prefix, err := Generate()
	if err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO api_keys (name, key_hash, prefix, scope)
		VALUES (?, ?, ?, ?)`, name, hash, prefix, scope); err != nil {
		return "", fmt.Errorf("apikeys: store: %w", err)
	}
	return k, nil
}

// Identity is the authenticated principal derived from a key.
type Identity struct {
	Scope       string
	AffiliateID int64 // valid when scope is affiliate
	IsAdmin     bool
	// Portal: a portal session signed in, not an API key. Only a portal
	// session may change where the affiliate is paid.
	Portal bool
}

// Authenticate verifies a bearer key and returns the identity. Updates
// last_used_at opportunistically (failure to update is non-fatal).
func Authenticate(ctx context.Context, db *sql.DB, key string) (*Identity, error) {
	if key == "" {
		return nil, ErrUnauthorized
	}
	hash := HashKey(key)
	var (
		scope     string
		revokedAt sql.NullString
		lastUsed  sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT scope, revoked_at, last_used_at FROM api_keys WHERE key_hash = ?`, hash).
		Scan(&scope, &revokedAt, &lastUsed)
	if err == sql.ErrNoRows {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("apikeys: lookup: %w", err)
	}
	if revokedAt.Valid {
		return nil, ErrUnauthorized // revoked
	}
	id := &Identity{Scope: scope, IsAdmin: IsAdminScope(scope)}
	if affID, ok := ScopeAffiliateID(scope); ok {
		id.AffiliateID = affID
		// An affiliate key works only while its affiliate is active, the
		// same rule portal sessions follow. A frozen, removed or missing
		// affiliate gets no access through a key.
		var status string
		err := db.QueryRowContext(ctx, `SELECT status FROM affiliates WHERE id = ?`, affID).Scan(&status)
		if err == sql.ErrNoRows || (err == nil && status != "active") {
			return nil, ErrUnauthorized
		}
		if err != nil {
			return nil, fmt.Errorf("apikeys: affiliate status: %w", err)
		}
	}
	// last_used_at: write ONLY when stale (>60s). The naive always-write
	// version took SQLite's single write lock on EVERY authenticated
	// request — serializing against click batches, ledger posts, and
	// maturation (the SQLITE_BUSY cliff). The fire-and-forget goroutine
	// before THAT raced TempDir cleanup and lock upgrades. Stale-gated
	// sync write: lock traffic drops ~1000x at 1 req/s per key, zero
	// detached goroutines, auth stays read-only in the common case.
	if !lastUsed.Valid || staleLastUsed(lastUsed.String, 60*time.Second) {
		_, _ = db.ExecContext(ctx, `
			UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?`,
			time.Now().UTC().Format(time.RFC3339), hash)
	}
	return id, nil
}

// staleLastUsed reports whether the stored RFC3339 timestamp is older than
// max. Unparseable values count as stale (self-heal).
func staleLastUsed(s string, max time.Duration) bool {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return true
	}
	return time.Since(t) > max
}

// ErrUnauthorized is returned for missing/unknown/revoked keys.
var ErrUnauthorized = fmt.Errorf("apikeys: unauthorized")

// Execer is the part of *sql.DB and *sql.Tx that RevokeAffiliate needs.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RevokeAffiliate revokes every live key scoped to one affiliate. Freezing
// or erasing an affiliate calls it, so unfreezing never brings an old key
// back: the operator issues a new one. Admin keys are never touched.
func RevokeAffiliate(ctx context.Context, q Execer, affiliateID int64) (int64, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = ? WHERE scope = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), AffiliateScope(affiliateID))
	if err != nil {
		return 0, fmt.Errorf("apikeys: revoke affiliate %d: %w", affiliateID, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Revoke marks a key revoked by prefix or id.
func Revoke(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("apikeys: revoke: %w", err)
	}
	return nil
}
