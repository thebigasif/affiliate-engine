package httpapi

import (
	"bytes"
	"testing"
)

// The GDPR export opener unseals what the portal sealed, and only for the
// affiliate the row is bound to.
func TestPayoutDetailsOpener(t *testing.T) {
	taxKey := DeriveTaxDocKey(bytes.Repeat([]byte("k"), 32))
	sealed, err := sealPayoutDetails(payoutKey(taxKey), 7, PayoutDetails{Method: "wise", WiseEmail: "me@wise.test"})
	if err != nil {
		t.Fatal(err)
	}
	open := PayoutDetailsOpener(taxKey)
	fields, ok := open(7, sealed)
	if !ok {
		t.Fatal("own row did not open")
	}
	if fields["method"] != "wise" || fields["wise_email"] != "me@wise.test" {
		t.Fatalf("fields = %v", fields)
	}
	if _, ok := open(8, sealed); ok {
		t.Fatal("row opened for another affiliate")
	}
	if _, ok := PayoutDetailsOpener(DeriveTaxDocKey(bytes.Repeat([]byte("x"), 32)))(7, sealed); ok {
		t.Fatal("row opened under another secret")
	}
}
