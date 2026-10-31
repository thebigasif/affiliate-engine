package partnermail_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

// credit books a commission row (and the journal it points at) credited
// at an RFC 3339 time.
func credit(t *testing.T, h *db.Handles, aff, amount int64, currency, at string) {
	t.Helper()
	key := fmt.Sprintf("pm:%d:%d:%s:%s", aff, amount, currency, at)
	var jid int64
	if err := h.DB.QueryRow(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', ?) RETURNING id`,
		key, at).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, currency, rate_snapshot, status, created_at)
		VALUES (?, ?, ?, ?, '{}', 'approved', ?)`, aff, jid, amount, currency, at); err != nil {
		t.Fatal(err)
	}
}

func deliverTopics(t *testing.T, h *db.Handles, sink *partnermail.Sink, topics ...string) {
	t.Helper()
	ctx := context.Background()
	for _, topic := range topics {
		rows, err := h.DB.Query(`SELECT payload FROM outbox_events WHERE topic = ? ORDER BY id`, topic)
		if err != nil {
			t.Fatal(err)
		}
		var evs []outbox.Event
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			evs = append(evs, outbox.Event{Topic: topic, Payload: []byte(p)})
		}
		rows.Close()
		if _, err := h.DB.Exec(`DELETE FROM outbox_events WHERE topic = ?`, topic); err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if err := sink.Deliver(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The Monday report: once, from 08:00 Monday, on last week's numbers
// compared with the week before, with the top partners.
func TestWeeklyReport(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`)
	must(`INSERT INTO affiliates (id, email, name) VALUES (7, 'lee@x.test', 'Lee Park'), (8, 'sam@x.test', '')`)
	// Last week: Mon 2026-09-21 to Sun 2026-09-27. The week before: 14-20.
	must(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks, conversions, commission_cents, currency) VALUES
		(20260922, 7, 1, 800, 3, 9000, 'USD'), (20260927, 8, 1, 400, 1, 8000, 'USD'),
		(20260915, 7, 1, 1000, 2, 6000, 'USD'), (20260928, 7, 1, 999, 9, 99900, 'USD')`)
	// The money comes from the commission rows, per currency (partner 8's
	// ¥5,000 and $30.00 stay apart).
	credit(t, h, 7, 9000, "USD", "2026-09-22T10:00:00Z")
	credit(t, h, 8, 3000, "USD", "2026-09-27T10:00:00Z")
	credit(t, h, 8, 5000, "JPY", "2026-09-27T11:00:00Z")
	credit(t, h, 7, 6000, "USD", "2026-09-15T10:00:00Z")
	credit(t, h, 7, 99900, "USD", "2026-09-28T10:00:00Z")
	c := &capture{}
	run := func(at time.Time) bool {
		t.Helper()
		sent, err := partnermail.WeeklyReport(ctx, h.DB, c, "https://app.acme.test/", "owner@acme.test", at)
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	mon := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	if run(mon.Add(7*time.Hour)) || run(mon.AddDate(0, 0, -1).Add(12*time.Hour)) {
		t.Fatal("sent before Monday 08:00")
	}
	if !run(mon.Add(9 * time.Hour)) {
		t.Fatal("not sent on Monday morning")
	}
	if run(mon.Add(10*time.Hour)) || run(mon.AddDate(0, 0, 1)) {
		t.Fatal("sent twice for one week")
	}
	m := c.sent[0]
	if m.To != "owner@acme.test" || m.Subject != "Acme Partners: last week, 1,200 clicks and 4 sales" || m.Tag != "weekly-report" {
		t.Fatalf("header: %+v", m)
	}
	for _, want := range []string{
		"last week (Sep 21 to Sep 27)",
		"- Clicks: 1,200 (up 20% on the week before)",
		"- Sales: 4 (up 100% on the week before)",
		"- Commission earned: 5,000 JPY and $120.00 (up 100% on the week before)",
		"1. Lee Park: $90.00 from 3 sales",
		"2. sam@x.test: 5,000 JPY + $30.00 from 1 sale",
		"https://app.acme.test/#/settings",
	} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, m.Text)
		}
	}
	// Thursday is past the catch-up window: a week missed is skipped.
	if run(mon.AddDate(0, 0, 10).Add(12 * time.Hour)) {
		t.Fatal("sent late in the week")
	}
}

// Tips go once to an active partner a week in with no clicks; the recap
// goes on the 1st to partners with activity or money waiting. Both skip
// partners who stopped program emails.
func TestPartnerTipsAndRecap(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ago := func(d int) string { return now.AddDate(0, 0, -d).Format("2006-01-02 15:04:05") }
	must(`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`)
	must(`INSERT INTO affiliates (id, email, name, created_at) VALUES
		(1, 'new@x.test', 'Nia', ?), (2, 'fresh@x.test', '', ?), (3, 'old@x.test', '', ?),
		(4, 'busy@x.test', 'Bo', ?), (5, 'quit@x.test', '', ?)`, ago(8), ago(3), ago(60), ago(9), ago(9))
	must(`UPDATE affiliates SET news_opt_out = 1 WHERE id = 5`)
	must(`INSERT INTO affiliate_codes (affiliate_id, code, is_primary) VALUES (1, 'nia', TRUE), (4, 'bo', TRUE)`)
	must(`INSERT INTO clicks (id, ts, day, month, affiliate_id) VALUES ('c1', '2026-09-20T10:00:00Z', 20260920, 202609, 4)`)
	must(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks, conversions, commission_cents, currency) VALUES
		(20260920, 4, 1, 1234, 2, 5000, 'USD'), (20260920, 5, 1, 10, 1, 1000, 'USD'), (20261001, 3, 1, 5, 0, 0, 'USD')`)
	credit(t, h, 4, 5000, "USD", "2026-09-20T10:00:00Z")
	credit(t, h, 5, 1000, "USD", "2026-09-20T10:00:00Z")

	n, err := partnermail.QueueTips(ctx, h.DB, now)
	if err != nil || n != 1 {
		t.Fatalf("tips queued %d (%v), want 1: partner 1 only", n, err)
	}
	if n, _ := partnermail.QueueTips(ctx, h.DB, now.AddDate(0, 0, 2)); n != 0 {
		t.Fatalf("tips queued again: %d", n)
	}
	n, err = partnermail.QueueRecaps(ctx, h.DB, now)
	if err != nil || n != 1 {
		t.Fatalf("recaps queued %d (%v), want 1: partner 4 only", n, err)
	}
	if n, _ := partnermail.QueueRecaps(ctx, h.DB, now.Add(time.Hour)); n != 0 {
		t.Fatalf("recap queued twice: %d", n)
	}
	if n, _ := partnermail.QueueRecaps(ctx, h.DB, now.AddDate(0, 0, 10)); n != 0 {
		t.Fatalf("recap queued past the catch-up window: %d", n)
	}

	c := &capture{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: c, PublicURL: "https://app.acme.test", NewsKey: partnermail.DeriveNewsKey([]byte("s")), Now: func() time.Time { return now }}
	deliverTopics(t, h, sink, partnermail.TopicTips, partnermail.TopicRecap)
	if len(c.sent) != 2 {
		t.Fatalf("sent %d, want 2: %+v", len(c.sent), c.sent)
	}
	tips, recap := c.sent[0], c.sent[1]
	if tips.To != "new@x.test" || tips.Subject != "3 easy ways to share your Acme Partners link" || tips.ListUnsubscribe == "" {
		t.Fatalf("tips header: %+v", tips)
	}
	for _, want := range []string{"Hi Nia,", "Your link: https://app.acme.test/r/nia", "1. Put it where", "Stop program emails", "/api/v1/portal/unsubscribe?t=1."} {
		if !strings.Contains(tips.Text, want) {
			t.Fatalf("tips lacks %q:\n%s", want, tips.Text)
		}
	}
	if recap.To != "busy@x.test" || recap.Subject != "Your September with Acme Partners: $50.00 earned" {
		t.Fatalf("recap header: %+v", recap)
	}
	for _, want := range []string{"Hi Bo,", "- Clicks: 1,234", "- Sales: 2", "- Commission earned: $50.00", "https://app.acme.test/r/bo"} {
		if !strings.Contains(recap.Text, want) {
			t.Fatalf("recap lacks %q:\n%s", want, recap.Text)
		}
	}

	// A partner who clicked before the tips went out gets none.
	must(`INSERT INTO affiliates (id, email, created_at) VALUES (6, 'late@x.test', ?)`, ago(10))
	if n, _ := partnermail.QueueTips(ctx, h.DB, now); n != 1 {
		t.Fatalf("tips for partner 6: %d", n)
	}
	must(`INSERT INTO clicks (id, ts, day, month, affiliate_id) VALUES ('c2', '2026-10-01T08:00:00Z', 20261001, 202610, 6)`)
	deliverTopics(t, h, sink, partnermail.TopicTips)
	if len(c.sent) != 2 {
		t.Fatalf("tips sent to a partner who has clicks now: %+v", c.sent[2:])
	}
}
