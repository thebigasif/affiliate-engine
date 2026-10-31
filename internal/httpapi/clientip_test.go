package httpapi

import (
	"crypto/tls"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestClientIP(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8", "fd00::/8")
	for _, tc := range []struct {
		name, remote, xff string
		trusted           []netip.Prefix
		want              string
	}{
		{"no proxies: peer wins, XFF ignored", "81.2.3.4:5555", "1.1.1.1", nil, "81.2.3.4"},
		{"untrusted peer: spoofed XFF ignored", "81.2.3.4:5555", "1.1.1.1", trusted, "81.2.3.4"},
		{"trusted peer: rightmost untrusted hop", "10.1.2.3:443", "198.51.100.7, 203.0.113.9", trusted, "203.0.113.9"},
		{"trusted chain skipped", "10.1.2.3:443", "203.0.113.9, 10.9.9.9", trusted, "203.0.113.9"},
		{"trusted peer, no XFF", "10.1.2.3:443", "", trusted, "10.1.2.3"},
		{"all hops trusted", "10.1.2.3:443", "10.5.5.5", trusted, "10.1.2.3"},
		{"garbage hop stops the walk", "10.1.2.3:443", "203.0.113.9, not-an-ip", trusted, "10.1.2.3"},
		{"hop with port", "10.1.2.3:443", "203.0.113.9:1234", trusted, "203.0.113.9"},
		{"v6 peer", "[2001:db8::1]:443", "", trusted, "2001:db8::1"},
		{"v6 trusted proxy", "[fd00::5]:443", "2001:db8::7", trusted, "2001:db8::7"},
		{"v4-mapped peer unmapped", "[::ffff:10.1.2.3]:443", "203.0.113.9", trusted, "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/r/x", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := ClientIP(r, tc.trusted); got != tc.want {
				t.Fatalf("ClientIP=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "81.2.3.4:1"
	r.Header.Set("X-Forwarded-Proto", "https")
	if requestIsHTTPS(r, trusted) {
		t.Fatal("X-Forwarded-Proto from an untrusted peer must be ignored")
	}
	r.RemoteAddr = "10.0.0.2:1"
	if !requestIsHTTPS(r, trusted) {
		t.Fatal("trusted proxy X-Forwarded-Proto=https ignored")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.TLS = &tls.ConnectionState{}
	if !requestIsHTTPS(r2, nil) {
		t.Fatal("direct TLS not detected")
	}
}

func TestSecureCookiesFromEnv(t *testing.T) {
	t.Setenv("AE_COOKIE_SECURE", "")
	t.Setenv("AE_PUBLIC_URL", "http://af.example.com")
	if SecureCookiesFromEnv() {
		t.Fatal("http public URL must not force Secure")
	}
	t.Setenv("AE_PUBLIC_URL", "https://af.example.com")
	if !SecureCookiesFromEnv() {
		t.Fatal("https public URL must force Secure")
	}
	t.Setenv("AE_PUBLIC_URL", "")
	t.Setenv("AE_COOKIE_SECURE", "1")
	if !SecureCookiesFromEnv() {
		t.Fatal("AE_COOKIE_SECURE=1 must force Secure")
	}
}

func TestTruncateUTF8(t *testing.T) {
	if got := truncateUTF8("abc", 10); got != "abc" {
		t.Fatalf("short string changed: %q", got)
	}
	// "é" is two bytes: a 3-byte cap must not split the second rune.
	if got := truncateUTF8("éé", 3); got != "é" {
		t.Fatalf("got %q want %q", got, "é")
	}
}
