package partnermail_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/mail"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

type capture struct{ sent []mail.Mail }

func (c *capture) Send(_ context.Context, m mail.Mail) error { c.sent = append(c.sent, m); return nil }

func TestMoney(t *testing.T) {
	for _, c := range []struct {
		cents int64
		cur   string
		want  string
	}{
		{123450, "USD", "$1,234.50"},
		{5, "usd", "$0.05"},
		{100000000, "EUR", "€1,000,000.00"},
		{1200, "GBP", "£12.00"},
		{1200, "CHF", "12.00 CHF"},
		{-2950, "USD", "-$29.50"},
		// ISO 4217 exponents: yen has no minor unit, dinar has three.
		{500000, "JPY", "500,000 JPY"},
		{1000, "KWD", "1.000 KWD"},
	} {
		if got := partnermail.Money(c.cents, c.cur); got != c.want {
			t.Errorf("Money(%d,%s) = %q, want %q", c.cents, c.cur, got, c.want)
		}
	}
}

// Only a paid payout is announced; the email names the program, amount
// and method, and links to the portal on the claimed domain.
func TestSink_PayoutPaid(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO campaigns (id, name) VALUES (1, 'Campaign Name')`)
	must(`INSERT INTO program_branding (campaign_id, brand_name, portal_domain) VALUES (1, 'Acme Partners', 'partners.acme.test')`)
	must(`INSERT INTO affiliates (id, email, name) VALUES (7, 'lee@x.test', 'Lee Park')`)
	must(`INSERT INTO payouts (id, affiliate_id, amount, currency, method, status) VALUES (1, 7, 123450, 'USD', 'paypal', 'paid')`)
	must(`INSERT INTO payouts (id, affiliate_id, amount, currency, method, status) VALUES (2, 7, 500, 'USD', 'manual', 'requested')`)

	c := &capture{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: c, PublicURL: "https://engine.test"}
	for _, id := range []int64{1, 2, 99} {
		p, _ := json.Marshal(map[string]int64{"payout_id": id})
		if err := sink.Deliver(ctx, outbox.Event{Topic: partnermail.TopicPayoutPaid, Payload: p}); err != nil {
			t.Fatalf("payout %d: %v", id, err)
		}
	}
	if len(c.sent) != 1 {
		t.Fatalf("sent %d emails, want 1 (only the paid payout)", len(c.sent))
	}
	m := c.sent[0]
	if m.To != "lee@x.test" || m.Subject != "Acme Partners paid you $1,234.50" || m.FromName != "Acme Partners" {
		t.Fatalf("header: %+v", m)
	}
	for _, want := range []string{"Hi Lee Park,", "paid you $1,234.50 by PayPal (payout #1)", "https://partners.acme.test/"} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, m.Text)
		}
	}
}

func TestUnsubscribeToken(t *testing.T) {
	k := partnermail.DeriveNewsKey([]byte("s"))
	tok := partnermail.UnsubscribeToken(k, 42)
	if id, ok := partnermail.ParseUnsubscribeToken(k, tok); !ok || id != 42 {
		t.Fatalf("round trip: %d %v", id, ok)
	}
	for _, bad := range []string{"", "42", "42.", "x." + tok[3:], "43" + tok[2:], tok + "0"} {
		if _, ok := partnermail.ParseUnsubscribeToken(k, bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, ok := partnermail.ParseUnsubscribeToken(partnermail.DeriveNewsKey([]byte("other")), tok); ok {
		t.Fatal("accepted a token from another deployment")
	}
}

// A declined applicant gets a short note, with the owner's reason; not
// if the application was approved since.
func TestSink_Rejected(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_applications (id, email, name, status) VALUES (1, 'no@x.test', 'Kim', 'rejected'), (2, 'yes@x.test', '', 'approved')`); err != nil {
		t.Fatal(err)
	}
	c := &capture{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: c, PublicURL: "https://engine.test"}
	for _, p := range []string{`{"application_id":1,"reason":"We only work with newsletters right now."}`, `{"application_id":2}`} {
		if err := sink.Deliver(ctx, outbox.Event{Topic: partnermail.TopicRejected, Payload: []byte(p)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(c.sent))
	}
	m := c.sent[0]
	if m.To != "no@x.test" || m.Subject != "Your application to Acme Partners" {
		t.Fatalf("header: %+v", m)
	}
	for _, want := range []string{"Hi Kim,", "can't accept your application right now", "We only work with newsletters right now."} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, m.Text)
		}
	}
}
