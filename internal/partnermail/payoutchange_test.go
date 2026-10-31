package partnermail_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

// The change notice goes to the partner's login address, shows only
// masked values, and says when payouts to the new details start.
func TestSink_PayoutDetailsChanged(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	for _, q := range []string{
		`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`,
		`INSERT INTO affiliates (id, email, name) VALUES (7, 'lee@x.test', 'Lee Park')`,
		`INSERT INTO affiliates (id, email, name, status) VALUES (8, 'erased+8@erased.invalid', '', 'removed')`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	c := &capture{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: c, PublicURL: "https://engine.test"}
	until := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	for _, ev := range []partnermail.PayoutDetailsChange{
		{AffiliateID: 7, Method: "paypal", Before: "PayPal: l***@x.test", After: "PayPal: n***@other.test", CoolingUntil: until},
		{AffiliateID: 8, Method: "paypal", After: "PayPal: n***@other.test"},
	} {
		p, _ := json.Marshal(ev)
		if err := sink.Deliver(ctx, outbox.Event{Topic: partnermail.TopicPayoutDetailsChanged, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.sent) != 1 {
		t.Fatalf("sent %d emails, want 1 (none to a removed partner)", len(c.sent))
	}
	m := c.sent[0]
	if m.To != "lee@x.test" || m.Subject != "Your payout details for Acme Partners changed" {
		t.Fatalf("header: %+v", m)
	}
	for _, want := range []string{"Before: PayPal: l***@x.test", "Now: PayPal: n***@other.test", "4 November 2026 at 10:00 UTC", "https://engine.test/"} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, m.Text)
		}
	}
}
