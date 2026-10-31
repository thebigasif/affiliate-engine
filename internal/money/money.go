// Package money converts integer minor units to a currency's major unit
// by its ISO 4217 exponent: JPY has no minor unit (5000 minor = ¥5000),
// KWD has three decimals (1000 minor = 1.000 KWD), most have two.
package money

import (
	"strconv"
	"strings"
)

// Exponent returns the ISO 4217 minor-unit exponent of a currency code
// (case-insensitive). Unknown codes use 2.
func Exponent(currency string) int {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	case "CLF", "UYW":
		return 4
	}
	return 2
}

// Format renders minor units as a plain decimal in the currency's major
// unit: Format(1234, "USD") = "12.34", Format(5000, "JPY") = "5000",
// Format(1000, "KWD") = "1.000". No grouping and no symbol, so the result
// is safe for payout files.
func Format(minor int64, currency string) string {
	return format(minor, Exponent(currency), "")
}

// Grouped is Format with thousands separators in the whole part
// ("1,234.50"), for people.
func Grouped(minor int64, currency string) string {
	return format(minor, Exponent(currency), ",")
}

func format(minor int64, exp int, sep string) string {
	neg := minor < 0
	// Work on the decimal string so math.MinInt64 does not overflow.
	digits := strconv.FormatInt(minor, 10)
	if neg {
		digits = digits[1:]
	}
	for len(digits) <= exp {
		digits = "0" + digits
	}
	whole, frac := digits[:len(digits)-exp], digits[len(digits)-exp:]
	if sep != "" {
		for i := len(whole) - 3; i > 0; i -= 3 {
			whole = whole[:i] + sep + whole[i:]
		}
	}
	out := whole
	if exp > 0 {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}
