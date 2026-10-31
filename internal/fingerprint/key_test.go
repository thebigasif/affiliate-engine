package fingerprint

// Keyed-identity tests. TestMain installs a fixed test key for the whole
// package (every hash assertion in this binary runs keyed, exactly like
// production after main.go calls SetHashKey).

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

var testHashKey = []byte("fingerprint-test-key-0123456789abcdef")

func TestMain(m *testing.M) {
	SetHashKey(testHashKey)
	os.Exit(m.Run())
}

// withKey runs fn under key, restoring the previous key afterwards.
func withKey(t *testing.T, key []byte, fn func()) {
	t.Helper()
	prev := append([]byte(nil), *hashKey.Load()...)
	SetHashKey(key)
	defer SetHashKey(prev)
	fn()
}

// The identity is HMAC(key, canonical) — NOT the unkeyed SHA-256 the old
// code stored (which is reversible for all 2^32 IPv4 addresses).
func TestNewIdentityIsKeyed(t *testing.T) {
	got := NewIdentity(ByIP, "1.2.3.4")
	canonical := fmt.Sprintf("%d:%s", len("1.2.3.4"), "1.2.3.4")

	unkeyed := sha256.Sum256([]byte(canonical))
	if bytes.Equal(got[:], unkeyed[:16]) {
		t.Fatal("identity hash is the unkeyed SHA-256 (brute-forceable)")
	}
	mac := hmac.New(sha256.New, testHashKey)
	mac.Write([]byte(canonical))
	if want := mac.Sum(nil)[:16]; !bytes.Equal(got[:], want) {
		t.Fatalf("identity = %x, want HMAC-SHA256(key, canonical)[:16] = %x", got, want)
	}
	if len(got.Hex()) != 32 {
		t.Fatalf("output format changed: hex length %d", len(got.Hex()))
	}
}

// Same key: deterministic. Different key: a different identity space.
func TestSetHashKeyChangesIdentity(t *testing.T) {
	base := NewIdentity(ByIP, "9.9.9.9")
	if NewIdentity(ByIP, "9.9.9.9") != base {
		t.Fatal("identity not deterministic under one key")
	}
	withKey(t, []byte("a-completely-different-key-material"), func() {
		if NewIdentity(ByIP, "9.9.9.9") == base {
			t.Fatal("identity unchanged after SetHashKey")
		}
	})
	if NewIdentity(ByIP, "9.9.9.9") != base {
		t.Fatal("restoring the key did not restore the identity")
	}
	// Empty key is ignored (never degrades to an empty-key HMAC).
	SetHashKey(nil)
	if NewIdentity(ByIP, "9.9.9.9") != base {
		t.Fatal("SetHashKey(nil) replaced the key")
	}
	// The caller's slice is copied: mutating it afterwards changes nothing.
	k := []byte("mutable-key-material-0123456789ab")
	withKey(t, k, func() {
		before := NewIdentity(ByIP, "9.9.9.9")
		k[0] ^= 0xff
		if NewIdentity(ByIP, "9.9.9.9") != before {
			t.Fatal("SetHashKey aliases the caller's slice")
		}
	})
}

// DeriveHashKey = HMAC-SHA256(secret, "ip-hash-v1"): 32 bytes, stable,
// secret-dependent, and never the secret itself.
func TestDeriveHashKey(t *testing.T) {
	s1 := []byte("deployment-secret-one-0123456789abcdef")
	s2 := []byte("deployment-secret-two-0123456789abcdef")
	k1 := DeriveHashKey(s1)
	if len(k1) != 32 {
		t.Fatalf("derived key length %d want 32", len(k1))
	}
	if !bytes.Equal(k1, DeriveHashKey(s1)) {
		t.Fatal("derivation not deterministic")
	}
	if bytes.Equal(k1, DeriveHashKey(s2)) {
		t.Fatal("different secrets derived the same key")
	}
	if bytes.Equal(k1, s1) {
		t.Fatal("derived key equals the secret")
	}
	mac := hmac.New(sha256.New, s1)
	mac.Write([]byte("ip-hash-v1"))
	if !bytes.Equal(k1, mac.Sum(nil)) {
		t.Fatal("derivation is not HMAC(secret, \"ip-hash-v1\")")
	}
}

// IPv6 rotation: every address inside one /64 maps to one subnet identity;
// neighbouring /64s do not. IPv4 groups by /24. Subnet identities never
// collide with per-address identities (domain separation).
func TestSubnetIdentity(t *testing.T) {
	same64 := []string{
		"2001:db8:1:2::1",
		"2001:db8:1:2:ffff:ffff:ffff:ffff",
		"2001:0db8:0001:0002:1234:5678:9abc:def0",
	}
	want := SubnetIdentity(same64[0])
	for _, ip := range same64[1:] {
		if SubnetIdentity(ip) != want {
			t.Errorf("%s: not grouped into its /64", ip)
		}
	}
	if SubnetIdentity("2001:db8:1:3::1") == want {
		t.Error("adjacent /64 collapsed into the same identity")
	}
	if SubnetIdentity("81.2.3.4") != SubnetIdentity("81.2.3.250") {
		t.Error("same /24 split")
	}
	if SubnetIdentity("81.2.3.4") == SubnetIdentity("81.2.4.4") {
		t.Error("different /24 collapsed")
	}
	if SubnetIdentity("::ffff:81.2.3.4") != SubnetIdentity("81.2.3.9") {
		t.Error("v4-mapped spelling not grouped with its /24")
	}
	if SubnetIdentity("81.2.3.4") == NewIdentity(ByIP, canonicalIP("81.2.3.4")) {
		t.Error("subnet identity equals the address identity")
	}
	first := SubnetIdentity("not-an-ip")
	if SubnetIdentity("not-an-ip") != first {
		t.Error("unparseable input not deterministic")
	}
}
