package money

import (
	"sort"
	"strings"
)

// Default is the currency of a money row that names none: the column
// default of commissions.currency and payouts.currency, and what an event
// without a currency is booked in.
const Default = "USD"

// Normalize upper-cases and trims a currency code; "" becomes Default.
func Normalize(currency string) string {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" {
		return Default
	}
	return c
}

// Valid reports whether currency is a well-formed ISO 4217 alphabetic
// code (three letters A-Z). It does not check the code is assigned.
func Valid(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	return true
}

// Display formats minor units for people, by the currency's exponent:
// $1,234.50, €12.00, £3.10, 12.00 CHF, 5,000 JPY or 1.000 KWD.
func Display(minor int64, currency string) string {
	neg := minor < 0
	n := Grouped(minor, currency)
	if neg {
		n = n[1:]
	}
	cur := Normalize(currency)
	switch cur {
	case "USD":
		n = "$" + n
	case "EUR":
		n = "€" + n
	case "GBP":
		n = "£" + n
	default:
		n = n + " " + cur
	}
	if neg {
		n = "-" + n
	}
	return n
}

// Total is one currency's amount in a per-currency summary.
type Total struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Display     string `json:"display"`
}

// Totals accumulates amounts per currency. Money in different currencies
// is never added together: a summary is a list, one entry per currency.
type Totals map[string]int64

// Add adds minor units to currency's total.
func (t Totals) Add(currency string, minor int64) {
	t[Normalize(currency)] += minor
}

// List returns the totals sorted by currency code, zero totals included.
// It is never nil (an empty summary encodes as []).
func (t Totals) List() []Total {
	out := make([]Total, 0, len(t))
	for c, v := range t {
		out = append(out, Total{Currency: c, AmountMinor: v, Display: Display(v, c)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// String renders the totals for people: "$12.50 + 5,000 JPY", or
// Display(0, Default) when empty.
func (t Totals) String() string {
	l := t.List()
	if len(l) == 0 {
		return Display(0, Default)
	}
	parts := make([]string, len(l))
	for i, x := range l {
		parts[i] = x.Display
	}
	return strings.Join(parts, " + ")
}

// Single returns the one currency's amount when the totals hold at most
// one currency (ok=false when they mix several). An empty set is
// (Default, 0, true). Legacy single-number API fields use it: they keep
// their value for single-currency programs and report 0 when mixed.
func (t Totals) Single() (currency string, minor int64, ok bool) {
	switch len(t) {
	case 0:
		return Default, 0, true
	case 1:
		for c, v := range t {
			return c, v, true
		}
	}
	return "", 0, false
}
