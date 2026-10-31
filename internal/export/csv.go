// Package export writes payout batches in provider-specific CSV shapes
// (manual rails for M1: PayPal mass-pay and Wise transfer list).
package export

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Row is one payout line of a manual-rail file. The caller selects and
// claims the rows (payouts.Runner.ClaimForExport applies the payment gate
// and marks them exported); this package only renders them.
type Row struct {
	PayoutID int64
	Email    string
	Amount   int64 // minor units
	Currency string
}

// PayPal writes a PayPal mass-pay CSV: one row per payout.
// Formula-injection safe: every cell goes through sanitize.
func PayPal(w io.Writer, rows []Row) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"email", "amount", "currency", "note_id"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			sanitize(r.Email), sanitize(Decimal(r.Amount, r.Currency)), sanitize(r.Currency),
			sanitize(fmt.Sprintf("payout-%d", r.PayoutID)),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Wise writes a Wise bulk-transfer CSV.
func Wise(w io.Writer, rows []Row) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"email", "amount", "currency", "reference"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			sanitize(r.Email), sanitize(Decimal(r.Amount, r.Currency)), sanitize(r.Currency),
			sanitize(fmt.Sprintf("AE-%d", r.PayoutID)),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Decimal renders minor units as a decimal string in the currency's major
// unit, by its ISO 4217 exponent: 1234 USD -> "12.34", 5000 JPY -> "5000",
// 1000 KWD -> "1.000". Exported for tests + CLI display.
func Decimal(minor int64, currency string) string {
	return money.Format(minor, currency)
}

// SanitizeCell is the exported form of sanitize (FR-64's ad-hoc
// exports share the rail files' formula-injection defense).
func SanitizeCell(s string) string { return sanitize(s) }

// sanitize defuses CSV formula injection: a cell starting with = + - @
// (=cmd|..., +1, -1, @SUM), with a tab, CR or LF (spreadsheets strip or
// split on them and then evaluate what follows), or with leading
// whitespace before any of = + - @, is prefixed with an apostrophe.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r', '\n':
		return "'" + s
	}
	if t := strings.TrimLeftFunc(s, unicode.IsSpace); t != s && t != "" &&
		strings.ContainsRune("=+-@", rune(t[0])) {
		return "'" + s
	}
	return s
}
