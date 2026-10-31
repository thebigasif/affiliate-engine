package money

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrMalformed rejects a decimal amount that is not a plain number in
// the currency's major unit.
var ErrMalformed = errors.New("money: malformed amount")

// ParseMinor converts a decimal amount in the currency's major unit (a
// Shopify Decimal such as "12.5", "1000.0", "7") to integer minor units
// by the currency's ISO 4217 exponent: ParseMinor("1000.0", "JPY") =
// 1000, ParseMinor("1.234", "KWD") = 1234, ParseMinor("12.5", "USD") =
// 1250. It never uses floating point.
//
// Accepted: an optional leading '-', ASCII digits, and an optional '.'
// followed by digits. Fraction digits beyond the exponent must be zero
// ("12.50" is fine for USD, "12.345" is not; "5000.0" is fine for JPY).
// Anything else — empty, separators, exponents, letters, overflow — is
// ErrMalformed: a money path never guesses.
func ParseMinor(amount, currency string) (int64, error) {
	s := strings.TrimSpace(amount)
	exp := Exponent(currency)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") || !digits(whole) || !digits(frac) {
		return 0, fmt.Errorf("%w: %q", ErrMalformed, amount)
	}
	if len(frac) > exp {
		if strings.TrimRight(frac[exp:], "0") != "" {
			return 0, fmt.Errorf("%w: %q has more than %d decimals for %s", ErrMalformed, amount, exp, Normalize(currency))
		}
		frac = frac[:exp]
	}
	for len(frac) < exp {
		frac += "0"
	}
	var v int64
	for _, c := range whole + frac {
		d := int64(c - '0')
		if v > (math.MaxInt64-d)/10 {
			return 0, fmt.Errorf("%w: %q overflows", ErrMalformed, amount)
		}
		v = v*10 + d
	}
	if neg {
		v = -v
	}
	return v, nil
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
