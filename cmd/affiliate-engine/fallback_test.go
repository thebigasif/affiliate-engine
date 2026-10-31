package main

import "testing"

func TestFallbackURLFromEnv(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", defaultFallbackURL},
		{"  ", defaultFallbackURL},
		{"https://shop.example/", "https://shop.example/"},
		{"http://localhost:3000/lost", "http://localhost:3000/lost"},
	} {
		got, err := fallbackURLFromEnv(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("fallbackURLFromEnv(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"shop.example", "/relative", "javascript:alert(1)", "ftp://x.example/", "https://", "https://user:pw@x.example/"} {
		if got, err := fallbackURLFromEnv(bad); err == nil {
			t.Errorf("fallbackURLFromEnv(%q) = %q, want error", bad, got)
		}
	}
}
