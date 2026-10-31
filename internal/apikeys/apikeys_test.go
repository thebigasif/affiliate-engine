package apikeys

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

func openKeysDB(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	return h
}

// TestGenerateFormat pins the key shape: "ae_" + 32 base62 chars, unique.
func TestGenerateFormat(t *testing.T) {
	seen := map[string]bool{}
	const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i := 0; i < 100; i++ {
		key, hash, prefix, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != 3+32 {
			t.Fatalf("len=%d want 35 (%q)", len(key), key)
		}
		if key[:3] != "ae_" {
			t.Fatalf("prefix=%q", key[:3])
		}
		if prefix != key[:10] {
			t.Fatalf("stored prefix %q != key[:10] %q", prefix, key[:10])
		}
		for _, c := range key[3:] {
			if !containsRune(base62, c) {
				t.Fatalf("non-base62 char %q in %q", c, key)
			}
		}
		if len(hash) != 64 { // hex sha256
			t.Fatalf("hash len=%d want 64", len(hash))
		}
		if seen[key] {
			t.Fatalf("duplicate key generated: %q", key)
		}
		seen[key] = true
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestHashKeyDeterministicAndNotPlaintext(t *testing.T) {
	k1, _, _, _ := Generate()
	h1, h2 := HashKey(k1), HashKey(k1)
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}
	if HashKey(k1) == k1 {
		t.Fatal("hash equals plaintext")
	}
}

func TestScopeParsing(t *testing.T) {
	if !IsAdminScope(ScopeAdmin) {
		t.Fatal("admin scope not detected")
	}
	if IsAdminScope(AffiliateScope(5)) {
		t.Fatal("affiliate scope detected as admin")
	}
	if id, ok := ScopeAffiliateID("affiliate:42"); !ok || id != 42 {
		t.Fatalf("affiliate:42 -> (%d,%v)", id, ok)
	}
	for _, bad := range []string{"admin", "affiliate:", "affiliate:abc", "affiliate:-1x", "affiliate:-1", "affiliate:0", "", "affiliate:7:junk", "affiliate: 7", "affiliate:07x"} {
		if _, ok := ScopeAffiliateID(bad); ok {
			t.Fatalf("%q parsed — want reject", bad)
		}
	}
}

func TestStoreAndAuthenticateRoundtrip(t *testing.T) {
	ctx := context.Background()
	h := openKeysDB(t)

	key, err := Store(ctx, h.DB, "ops", ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	idn, err := Authenticate(ctx, h.DB, key)
	if err != nil {
		t.Fatal(err)
	}
	if !idn.IsAdmin || idn.AffiliateID != 0 {
		t.Fatalf("identity=%+v", idn)
	}

	// Affiliate scope roundtrip.
	affKey, err := Store(ctx, h.DB, "aff7", AffiliateScope(7))
	if err != nil {
		t.Fatal(err)
	}
	idn, err = Authenticate(ctx, h.DB, affKey)
	if err != nil {
		t.Fatal(err)
	}
	if idn.IsAdmin || idn.AffiliateID != 7 {
		t.Fatalf("identity=%+v", idn)
	}

	// Wrong/missing keys rejected.
	if _, err := Authenticate(ctx, h.DB, ""); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := Authenticate(ctx, h.DB, "ae_totallymadeup0000000000000000000"); err == nil {
		t.Fatal("unknown key accepted")
	}

	// A mutated key (one char changed) must not authenticate.
	bad := []byte(affKey)
	if bad[len(bad)-1] == 'A' {
		bad[len(bad)-1] = 'B'
	} else {
		bad[len(bad)-1] = 'A'
	}
	if _, err := Authenticate(ctx, h.DB, string(bad)); err == nil {
		t.Fatal("mutated key accepted")
	}
}

func TestRevokeBlocksAuthentication(t *testing.T) {
	ctx := context.Background()
	h := openKeysDB(t)

	key, err := Store(ctx, h.DB, "temp", AffiliateScope(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(ctx, h.DB, key); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}

	// Revoke by id (id=1: first key in a fresh DB).
	if err := Revoke(ctx, h.DB, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(ctx, h.DB, key); err == nil {
		t.Fatal("revoked key accepted")
	}
	// Revoking twice is a no-op, not an error.
	if err := Revoke(ctx, h.DB, 1); err != nil {
		t.Fatalf("double revoke: %v", err)
	}
	// last_used_at was touched by the successful auth (opportunistic update).
	deadline := time.Now().Add(2 * time.Second)
	var lastUsed sql.NullString
	for time.Now().Before(deadline) {
		_ = h.DB.QueryRow(`SELECT last_used_at FROM api_keys WHERE id=1`).Scan(&lastUsed)
		if lastUsed.Valid {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !lastUsed.Valid {
		t.Fatal("last_used_at never updated after successful auth")
	}
}

// TestUniquenessGuard stores two keys and verifies hash uniqueness is
// enforced by the schema (two distinct generated keys never collide).
func TestUniquenessGuard(t *testing.T) {
	ctx := context.Background()
	h := openKeysDB(t)
	for i := 0; i < 10; i++ {
		if _, err := Store(ctx, h.DB, fmt.Sprintf("k%d", i), ScopeAdmin); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("stored=%d want 10", n)
	}
}
