package config

import (
	"net/netip"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 10.0.0.0/8, 203.0.113.7 ,, fd00::/8,10.1.2.3/16")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "203.0.113.7/32", "fd00::/8", "10.1.0.0/16"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i, w := range want {
		if got[i] != netip.MustParsePrefix(w) {
			t.Fatalf("entry %d = %v want %s", i, got[i], w)
		}
	}
	if p, err := ParseTrustedProxies(""); err != nil || len(p) != 0 {
		t.Fatalf("empty: %v %v", p, err)
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0.1/abc"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
