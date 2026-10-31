package partnermail_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

// A partner with a payout waiting and no payout details is asked once a
// week; one who has details, is paused, or has nothing waiting is not.
// The email names what's waiting and signs them in to the form.
func TestAskPayoutDetails(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`)
	must(`INSERT INTO affiliates (id, email, name, status) VALUES (7, 'lee@x.test', 'Lee', 'active'), (8, 'has@x.test', '', 'active'), (9, 'paused@x.test', '', 'frozen'), (10, 'none@x.test', '', 'active')`)
	must(`INSERT INTO payouts (affiliate_id, amount, currency, method, status) VALUES (7, 12000, 'USD', 'paypal', 'requested'), (7, 5000, 'EUR', 'paypal', 'requested'), (7, 999, 'USD', 'paypal', 'paid'), (8, 5000, 'USD', 'paypal', 'requested'), (9, 5000, 'USD', 'paypal', 'requested'), (10, 5000, 'USD', 'paypal', 'paid')`)
	must(`INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (8, 'paypal', ?)`, []byte("x"))

	t0 := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ask := func(at time.Time) int {
		t.Helper()
		n, err := partnermail.AskPayoutDetails(ctx, h.DB, at)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := ask(t0); n != 1 {
		t.Fatalf("asked %d, want 1 (only partner 7)", n)
	}
	if n := ask(t0.Add(3 * 24 * time.Hour)); n != 0 {
		t.Fatalf("asked again within a week: %d", n)
	}
	if got := partnermail.PayoutDetailsAsked(ctx, h.DB, []int64{7, 8, 7}); got[7] != t0.Format(time.RFC3339) || got[8] != "" {
		t.Fatalf("asked map: %v", got)
	}

	// The queued email.
	c := &capture{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: c, PublicURL: "https://app.acme.test", Now: func() time.Time { return t0 }}
	deliver := func() {
		t.Helper()
		rows, err := h.DB.Query(`SELECT topic, payload FROM outbox_events WHERE topic = ? ORDER BY id`, partnermail.TopicLoginLink)
		if err != nil {
			t.Fatal(err)
		}
		var evs []outbox.Event
		for rows.Next() {
			var e outbox.Event
			var p string
			if err := rows.Scan(&e.Topic, &p); err != nil {
				t.Fatal(err)
			}
			e.Payload = []byte(p)
			evs = append(evs, e)
		}
		rows.Close()
		must(`DELETE FROM outbox_events`)
		for _, e := range evs {
			if err := sink.Deliver(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	deliver()
	if len(c.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(c.sent))
	}
	m := c.sent[0]
	if m.To != "lee@x.test" || m.Subject != "Where should Acme Partners send your money?" || m.Tag != "payout-details" {
		t.Fatalf("header: %+v", m)
	}
	for _, want := range []string{"Hi Lee,", "Acme Partners has €50.00 and $120.00 ready for you", "How you get paid", "https://app.acme.test/#grant=aps_", "&to=payto"} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, m.Text)
		}
	}

	// A week later, still missing: asked again. Added before the email
	// went out: nothing is sent.
	if n := ask(t0.Add(8 * 24 * time.Hour)); n != 1 {
		t.Fatalf("not asked again after a week: %d", n)
	}
	must(`INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (7, 'wise', ?)`, []byte("x"))
	deliver()
	if len(c.sent) != 1 {
		t.Fatalf("emailed a partner who has since added details: %+v", c.sent[1:])
	}
	if n := ask(t0.Add(30 * 24 * time.Hour)); n != 0 {
		t.Fatalf("asked a partner with details: %d", n)
	}
}
