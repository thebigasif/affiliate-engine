package export

import "testing"

// Formula-injection guard: every trigger prefix is neutralized, plain
// values pass through untouched.
func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"pay@me.dev":         "pay@me.dev",
		"USD":                "USD",
		"12.34":              "12.34",
		"payout-7":           "payout-7",
		" plain":             " plain",
		"=cmd|' /C calc'!A0": "'=cmd|' /C calc'!A0",
		"+1":                 "'+1",
		"-1":                 "'-1",
		"@SUM(A1)":           "'@SUM(A1)",
		"\t=1+1":             "'\t=1+1",
		"\tplain":            "'\tplain",
		"\r=1+1":             "'\r=1+1",
		"\n=1+1":             "'\n=1+1",
		"  =HYPERLINK(1)":    "'  =HYPERLINK(1)",
		" +1":                "' +1",
		"   @x":              "'   @x",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q)=%q want %q", in, got, want)
		}
	}
}
