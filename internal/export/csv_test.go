package export_test

import (
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/export"
)

// rows are what payouts.Runner.ClaimForExport hands the writers; the
// selection and claim are covered in internal/payouts (export_test.go).
var rows = []export.Row{
	{PayoutID: 7, Email: "pay@me.dev", Amount: 123456, Currency: "USD"},
	{PayoutID: 8, Email: "=EVIL(id)", Amount: 100, Currency: "USD"},
	{PayoutID: 10, Email: " @SUM(A1)", Amount: 200, Currency: "EUR"},
}

// E1: PayPal CSV renders payouts with correct decimal + note id.
func TestExport_PayPal(t *testing.T) {
	var sb strings.Builder
	if err := export.PayPal(&sb, rows); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "email,amount,currency,note_id\n") {
		t.Fatalf("header:\n%s", out)
	}
	if !strings.Contains(out, "pay@me.dev,1234.56,USD,payout-7") {
		t.Fatalf("paypal row missing:\n%s", out)
	}
	// Formula injection sanitized.
	if strings.Contains(out, "\"=EVIL") || strings.Contains(out, ",=EVIL") {
		t.Fatalf("formula injection not sanitized:\n%s", out)
	}
	if !strings.Contains(out, "'=EVIL(id)") {
		t.Fatalf("sanitized cell missing:\n%s", out)
	}
	// Leading whitespace before a formula char is neutralized too.
	if !strings.Contains(out, "' @SUM(A1),2.00,EUR,payout-10") {
		t.Fatalf("whitespace-prefixed formula not sanitized:\n%s", out)
	}
}

// E2: Wise CSV same data, different reference shape.
func TestExport_Wise(t *testing.T) {
	var sb strings.Builder
	if err := export.Wise(&sb, rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "pay@me.dev,1234.56,USD,AE-7") {
		t.Fatalf("wise row missing:\n%s", sb.String())
	}
}

// E3: decimal edge cases.
func TestExport_Decimal(t *testing.T) {
	cases := map[int64]string{
		0: "0.00", 5: "0.05", 100: "1.00", 123456: "1234.56", 9: "0.09",
	}
	for in, want := range cases {
		if got := export.Decimal(in, "USD"); got != want {
			t.Fatalf("decimal(%d)=%s want %s", in, got, want)
		}
	}
	if export.Decimal(-250, "USD") != "-2.50" {
		t.Fatal("negative decimal broken")
	}
}

// E4: amounts follow the currency's ISO 4217 exponent. ¥5000 is 5000
// minor units (not ¥50.00); 1.000 KWD is 1000 minor units (not 10.00).
func TestExport_CurrencyExponent(t *testing.T) {
	rows := []export.Row{
		{PayoutID: 1, Email: "jp@me.dev", Amount: 5000, Currency: "JPY"},
		{PayoutID: 2, Email: "kw@me.dev", Amount: 1000, Currency: "KWD"},
	}
	for name, write := range map[string]func(*strings.Builder) error{
		"paypal": func(sb *strings.Builder) error { return export.PayPal(sb, rows) },
		"wise":   func(sb *strings.Builder) error { return export.Wise(sb, rows) },
	} {
		var sb strings.Builder
		if err := write(&sb); err != nil {
			t.Fatal(err)
		}
		out := sb.String()
		if !strings.Contains(out, "jp@me.dev,5000,JPY,") {
			t.Errorf("%s: JPY amount wrong:\n%s", name, out)
		}
		if !strings.Contains(out, "kw@me.dev,1.000,KWD,") {
			t.Errorf("%s: KWD amount wrong:\n%s", name, out)
		}
	}
}
