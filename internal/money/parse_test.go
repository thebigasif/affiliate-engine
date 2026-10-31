package money

import (
	"errors"
	"testing"
)

func TestParseMinorByExponent(t *testing.T) {
	ok := []struct {
		in, cur string
		want    int64
	}{
		{"12.34", "USD", 1234},
		{"12.5", "USD", 1250},
		{"12", "USD", 1200},
		{"12.340", "USD", 1234},
		{"0.05", "EUR", 5},
		{"-3.00", "USD", -300},
		{"5000", "JPY", 5000},
		{"5000.0", "JPY", 5000},
		{"5000.00", "JPY", 5000},
		{"1.234", "KWD", 1234},
		{"1.2", "KWD", 1200},
		{"10", "KWD", 10000},
		{"0", "KWD", 0},
		{"9223372036854775807", "JPY", 9223372036854775807},
	}
	for _, c := range ok {
		got, err := ParseMinor(c.in, c.cur)
		if err != nil || got != c.want {
			t.Errorf("ParseMinor(%q, %s) = %d, %v; want %d", c.in, c.cur, got, err, c.want)
		}
	}
	bad := []struct{ in, cur string }{
		{"", "USD"},
		{"abc", "USD"},
		{"1,234.56", "USD"},
		{"12.345", "USD"},
		{"5000.5", "JPY"},
		{"1.2345", "KWD"},
		{"1e3", "USD"},
		{"12.", "USD"},
		{".5", "USD"},
		{"--1", "USD"},
		{"92233720368547758.08", "USD"},
		{" 1 2", "USD"},
	}
	for _, c := range bad {
		if v, err := ParseMinor(c.in, c.cur); !errors.Is(err, ErrMalformed) {
			t.Errorf("ParseMinor(%q, %s) = %d, %v; want ErrMalformed", c.in, c.cur, v, err)
		}
	}
	// Round trip with Format for every exponent class.
	for _, c := range []struct {
		minor int64
		cur   string
	}{{123456, "USD"}, {5000, "JPY"}, {1234, "KWD"}, {-77, "EUR"}} {
		got, err := ParseMinor(Format(c.minor, c.cur), c.cur)
		if err != nil || got != c.minor {
			t.Errorf("round trip %d %s: %d %v", c.minor, c.cur, got, err)
		}
	}
}
