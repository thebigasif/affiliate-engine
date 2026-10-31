package money_test

import (
	"math"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

func TestFormat_ByExponent(t *testing.T) {
	cases := []struct {
		minor int64
		cur   string
		want  string
	}{
		{1234, "USD", "12.34"},
		{5, "EUR", "0.05"},
		{0, "USD", "0.00"},
		{-250, "USD", "-2.50"},
		{5000, "JPY", "5000"},
		{5000, "jpy", "5000"},
		{-7, "KRW", "-7"},
		{1000, "KWD", "1.000"},
		{12345, "BHD", "12.345"},
		{5, "TND", "0.005"},
		{1234, "", "12.34"},
		{math.MinInt64, "USD", "-92233720368547758.08"},
	}
	for _, c := range cases {
		if got := money.Format(c.minor, c.cur); got != c.want {
			t.Errorf("Format(%d, %q) = %q, want %q", c.minor, c.cur, got, c.want)
		}
	}
}

func TestGrouped(t *testing.T) {
	cases := []struct {
		minor int64
		cur   string
		want  string
	}{
		{123450, "USD", "1,234.50"},
		{1234567, "JPY", "1,234,567"},
		{1234567, "KWD", "1,234.567"},
		{-100000, "USD", "-1,000.00"},
	}
	for _, c := range cases {
		if got := money.Grouped(c.minor, c.cur); got != c.want {
			t.Errorf("Grouped(%d, %q) = %q, want %q", c.minor, c.cur, got, c.want)
		}
	}
}
