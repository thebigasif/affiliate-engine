package privacy_test

// NFR-7 write-time IP anonymization (AE_IP_MODE). The redirect must
// never break on a malformed RemoteAddr (fail-open, mirroring the bot
// classifier); truncation degrades unique-counting and velocity rules
// to subnet granularity — by design, and asserted here.

import (
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/privacy"
)

func TestIPModeParse(t *testing.T) {
	cases := map[string]struct {
		want privacy.IPMode
		err  bool
	}{
		"":          {privacy.IPModeOff, false},
		"off":       {privacy.IPModeOff, false},
		"full":      {privacy.IPModeOff, false},
		"FULL":      {privacy.IPModeOff, false},
		"trunc24":   {privacy.IPModeTrunc24, false},
		"trunc":     {privacy.IPModeTrunc24, false},
		"anonymize": {privacy.IPModeTrunc24, false},
		"Anonymize": {privacy.IPModeTrunc24, false},
		" trunc24 ": {privacy.IPModeTrunc24, false},
		"md5":       {privacy.IPModeOff, true},
		"zero":      {privacy.IPModeOff, true},
	}
	for in, c := range cases {
		m, err := privacy.ParseIPMode(in)
		if c.err {
			if err == nil {
				t.Errorf("ParseIPMode(%q): want error", in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseIPMode(%q): %v", in, err)
			continue
		}
		if m != c.want {
			t.Errorf("ParseIPMode(%q) = %d want %d", in, m, c.want)
		}
	}
}

func TestAnonymizeIP(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"192.168.14.233", "192.168.14.0"},
		{"10.0.0.1", "10.0.0.0"},
		{"2001:db8:85a3:1234:8a2e:370:7334:9abc", "2001:db8:85a3::"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1::"},
	}
	for _, c := range cases {
		if got := privacy.AnonymizeIP(privacy.IPModeTrunc24, c.in); got != c.want {
			t.Errorf("AnonymizeIP(%q) = %q want %q", c.in, got, c.want)
		}
	}
	// Off mode is identity.
	if got := privacy.AnonymizeIP(privacy.IPModeOff, "192.168.14.233"); got != "192.168.14.233" {
		t.Errorf("IPModeOff mutated the ip: %q", got)
	}
	// Unparseable input passes through in EVERY mode (fail-open: the
	// redirect must never 500 on a garbage RemoteAddr).
	for _, m := range []privacy.IPMode{privacy.IPModeOff, privacy.IPModeTrunc24} {
		for _, junk := range []string{"", "not-an-ip", "999.999.999.999", "::gg::"} {
			if got := privacy.AnonymizeIP(m, junk); got != junk {
				t.Errorf("AnonymizeIP(%d, %q) = %q — must pass through unchanged", m, junk, got)
			}
		}
	}
	// Subnet neighbours collapse; different subnets do not (the unique-
	// counting contract under truncation).
	a := privacy.AnonymizeIP(privacy.IPModeTrunc24, "81.2.3.4")
	b := privacy.AnonymizeIP(privacy.IPModeTrunc24, "81.2.3.250")
	c := privacy.AnonymizeIP(privacy.IPModeTrunc24, "81.2.4.1")
	if a != b {
		t.Errorf("same /24 diverged: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different /24 collapsed: %q vs %q", a, c)
	}
}
