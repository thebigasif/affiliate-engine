package partnermail_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

// The owner's email: nothing when nothing waits; one when something does;
// not again for the same pile; a new item waits for the daily gap; off
// means off.
func TestOwnerDigest(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	c := &capture{}
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`)
	t0 := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	run := func(at time.Time) bool {
		t.Helper()
		sent, err := partnermail.OwnerDigest(ctx, h.DB, c, "https://app.acme.test/", "owner@acme.test", at)
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}

	if run(t0) {
		t.Fatal("sent with nothing waiting")
	}
	must(`INSERT INTO affiliate_applications (email, name) VALUES ('a@x.test', 'A'), ('b@x.test', 'B')`)
	must(`INSERT INTO affiliates (id, email) VALUES (7, 'p@x.test')`)
	must(`INSERT INTO payouts (affiliate_id, amount, method, status) VALUES (7, 5000, 'paypal', 'requested')`)
	if !run(t0) {
		t.Fatal("not sent with applications and a payout waiting")
	}
	m := c.sent[0]
	if m.To != "owner@acme.test" || m.FromName != "Acme Partners" || m.Subject != "Acme Partners: 2 applications, 1 payout to send" {
		t.Fatalf("header: %+v", m)
	}
	for _, want := range []string{
		"- 2 people applied to become partners: https://app.acme.test/#/applications",
		"- 1 payout is ready to send: https://app.acme.test/#/payouts",
		"Turn it off in Settings: https://app.acme.test/#/settings",
	} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, m.Text)
		}
	}
	if strings.Contains(m.Text, "held") || strings.Contains(m.Text, "tax form") {
		t.Fatalf("lists items with nothing waiting:\n%s", m.Text)
	}
	// Same pile a day later: not again.
	if run(t0.Add(30 * time.Hour)) {
		t.Fatal("sent again for the same pile")
	}
	// Something new, but within the daily gap: wait.
	must(`INSERT INTO affiliate_applications (email, name) VALUES ('c@x.test', 'C')`)
	if run(t0.Add(2 * time.Hour)) {
		t.Fatal("sent twice in one day")
	}
	if !run(t0.Add(21*time.Hour)) || !strings.Contains(c.sent[1].Subject, "3 applications") {
		t.Fatalf("new application after a day not sent: %v", c.sent)
	}
	// Turned off, or no address: nothing.
	if err := partnermail.SaveOwnerNotify(ctx, h.DB, partnermail.OwnerNotify{Email: "owner@acme.test", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	must(`INSERT INTO affiliate_applications (email, name) VALUES ('d@x.test', 'D')`)
	if run(t0.Add(60 * time.Hour)) {
		t.Fatal("sent while turned off")
	}
	if err := partnermail.SaveOwnerNotify(ctx, h.DB, partnermail.OwnerNotify{Email: "", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if sent, _ := partnermail.OwnerDigest(ctx, h.DB, c, "https://app.acme.test", "", t0.Add(60*time.Hour)); sent {
		t.Fatal("sent with no address")
	}
	// A saved address wins over the default.
	if err := partnermail.SaveOwnerNotify(ctx, h.DB, partnermail.OwnerNotify{Email: "boss@acme.test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if !run(t0.Add(60*time.Hour)) || c.sent[len(c.sent)-1].To != "boss@acme.test" {
		t.Fatalf("saved address not used: %+v", c.sent[len(c.sent)-1])
	}
}
