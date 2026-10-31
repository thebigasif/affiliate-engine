package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/mail"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

type captureMailer struct {
	mu   sync.Mutex
	sent []mail.Mail
}

func (c *captureMailer) Send(_ context.Context, m mail.Mail) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

// flushMail delivers every queued mail event through the sink, the way the
// outbox dispatcher does, and marks it delivered.
func flushMail(t *testing.T, h *db.Handles, sink *partnermail.Sink) int {
	t.Helper()
	rows, err := h.DB.Query(`SELECT id, topic, payload FROM outbox_events WHERE topic LIKE 'mail.%' AND delivered_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var evs []outbox.Event
	for rows.Next() {
		var e outbox.Event
		var p string
		if err := rows.Scan(&e.ID, &e.Topic, &p); err != nil {
			t.Fatal(err)
		}
		e.Payload = json.RawMessage(p)
		evs = append(evs, e)
	}
	rows.Close()
	for _, e := range evs {
		if err := sink.Deliver(context.Background(), e); err != nil {
			t.Fatalf("deliver %s: %v", e.Topic, err)
		}
		h.DB.Exec(`UPDATE outbox_events SET delivered_at = CURRENT_TIMESTAMP WHERE id = ?`, e.ID)
	}
	return len(evs)
}

var grantRe = regexp.MustCompile(`#grant=(aps_[0-9a-f]{64})`)

// Approving with email on queues a login-link email; the emailed link
// signs the affiliate in, once, and carries their referral link.
func TestPartnerMail_ApprovalEmailSignsIn(t *testing.T) {
	srv, h, adminKey := portalEnvWith(t, func(d *httpapi.APIDeps) { d.MailOn = true })
	mc := &captureMailer{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: mc, PublicURL: "https://partners.acme.test/"}
	h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'Acme Partners')`)
	h.DB.Exec(`UPDATE campaigns SET commission_months = 12, attribution_window_days = 60 WHERE id = 1`)
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value) VALUES (1, NULL, 'percent', 1250)`); err != nil {
		t.Fatal(err)
	}

	if code, out := postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"Jo@Example.com","name":"Jo Bloggs"}`, ""); code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	var appID int64
	h.DB.QueryRow(`SELECT id FROM affiliate_applications WHERE email = 'jo@example.com'`).Scan(&appID)
	code, out := postJSON(t, srv.URL+"/api/v1/admin/portal/review", `{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
	if code != 200 || out["emailed"] != true {
		t.Fatalf("approve: %d %v", code, out)
	}
	if n := flushMail(t, h, sink); n != 1 {
		t.Fatalf("queued %d emails, want 1", n)
	}
	if len(mc.sent) != 1 {
		t.Fatalf("sent %d emails", len(mc.sent))
	}
	m := mc.sent[0]
	if m.To != "jo@example.com" || m.FromName != "Acme Partners" || !strings.Contains(m.Subject, "Acme Partners") {
		t.Fatalf("email header: %+v", m)
	}
	if !strings.HasPrefix(m.Text, "Hi Jo Bloggs,") || !strings.Contains(m.Text, "https://partners.acme.test/r/jobloggs") {
		t.Fatalf("email body:\n%s", m.Text)
	}
	// The link to share and the deal come before how to sign in.
	for _, want := range []string{
		"12.5% of every payment from customers you send, for their first 12 months",
		"A 60-day cookie",
		"Paid once you've earned $50.00",
	} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("deal line %q missing:\n%s", want, m.Text)
		}
	}
	if strings.Index(m.Text, "/r/jobloggs") > strings.Index(m.Text, "#grant=") {
		t.Fatalf("referral link should come before the sign-in link:\n%s", m.Text)
	}
	g := grantRe.FindStringSubmatch(m.Text)
	if g == nil || !strings.Contains(m.Text, "https://partners.acme.test/#grant=") {
		t.Fatalf("no login link in:\n%s", m.Text)
	}
	if code, out := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+g[1]+`"}`, ""); code != 200 {
		t.Fatalf("login with emailed grant: %d %v", code, out)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+g[1]+`"}`, ""); code == 200 {
		t.Fatal("emailed grant worked twice")
	}
	// The admin still gets a link to copy.
	if s, _ := out["login_grant"].(string); !strings.HasPrefix(s, "aps_") {
		t.Fatalf("admin grant missing: %v", out)
	}
}

// "Email me a login link": same answer for strangers and affiliates, one
// email per address per window, nothing for a frozen affiliate.
func TestPartnerMail_LoginLinkRequest(t *testing.T) {
	srv, h, _ := portalEnvWith(t, func(d *httpapi.APIDeps) { d.MailOn = true })
	mc := &captureMailer{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: mc, PublicURL: "https://p.test"}
	h.DB.Exec(`INSERT INTO affiliates (email, name) VALUES ('Sam@X.test', 'Sam')`)
	h.DB.Exec(`INSERT INTO affiliates (email, name, status) VALUES ('frozen@x.test', 'F', 'frozen')`)

	for _, body := range []string{`{"email":"nobody@x.test"}`, `{"email":" SAM@x.test "}`, `{"email":"sam@x.test"}`, `{"email":"frozen@x.test"}`} {
		code, out := postJSON(t, srv.URL+"/api/v1/portal/login-link", body, "")
		if code != 200 || out["status"] != "sent" {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	if n := flushMail(t, h, sink); n != 1 {
		t.Fatalf("queued %d emails, want 1 (stranger, repeat and frozen queue nothing)", n)
	}
	if len(mc.sent) != 1 || mc.sent[0].To != "Sam@X.test" || !strings.Contains(mc.sent[0].Text, "expires in 1 hour") {
		t.Fatalf("sent: %+v", mc.sent)
	}
	g := grantRe.FindStringSubmatch(mc.sent[0].Text)
	if g == nil {
		t.Fatalf("no link:\n%s", mc.sent[0].Text)
	}
	if code, out := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+g[1]+`"}`, ""); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}

	// Frozen after the email was queued: the sink sends nothing.
	h.DB.Exec(`INSERT INTO affiliates (email, name) VALUES ('late@x.test', 'Late')`)
	postJSON(t, srv.URL+"/api/v1/portal/login-link", `{"email":"late@x.test"}`, "")
	h.DB.Exec(`UPDATE affiliates SET status = 'frozen' WHERE email = 'late@x.test'`)
	flushMail(t, h, sink)
	if len(mc.sent) != 1 {
		t.Fatalf("emailed a frozen affiliate: %+v", mc.sent[1:])
	}

	for _, body := range []string{`{}`, `{"email":"no-at-sign"}`, `not json`} {
		if code, _ := postJSON(t, srv.URL+"/api/v1/portal/login-link", body, ""); code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", body, code)
		}
	}
}

// Email off: approval queues nothing and the request form says so.
func TestPartnerMail_Off(t *testing.T) {
	srv, h, adminKey := portalEnv(t)
	postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"a@x.test","name":"A"}`, "")
	var appID int64
	h.DB.QueryRow(`SELECT id FROM affiliate_applications WHERE email = 'a@x.test'`).Scan(&appID)
	_, out := postJSON(t, srv.URL+"/api/v1/admin/portal/review", `{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
	if out["emailed"] != false {
		t.Fatalf("emailed with mail off: %v", out)
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE topic LIKE 'mail.%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d mail events with mail off", n)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/login-link", `{"email":"a@x.test"}`, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("login-link with mail off: %d, want 503", code)
	}
}

// Program news: queued for active partners who haven't unsubscribed; the
// email carries a working two-step unsubscribe link.
func TestPartnerNews_SendAndUnsubscribe(t *testing.T) {
	key := partnermail.DeriveNewsKey([]byte("test-secret"))
	srv, h, adminKey := portalEnvWith(t, func(d *httpapi.APIDeps) { d.MailOn = true; d.NewsKey = key })
	mc := &captureMailer{}
	sink := &partnermail.Sink{DB: h.DB, Mailer: mc, PublicURL: srv.URL, NewsKey: key}
	h.DB.Exec(`INSERT INTO affiliates (email, name) VALUES ('a@x.test', 'Ann Lee'), ('b@x.test', 'Bo'), ('c@x.test', 'Cy')`)
	h.DB.Exec(`UPDATE affiliates SET status = 'frozen' WHERE email = 'c@x.test'`)

	for _, bad := range []string{`{"subject":"","body":"x"}`, `{"subject":"Hi","body":""}`} {
		if code, _ := postJSON(t, srv.URL+"/api/v1/admin/partners/message", bad, adminKey); code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", bad, code)
		}
	}
	code, out := postJSON(t, srv.URL+"/api/v1/admin/partners/message", `{"subject":"40% this month","body":"Commission is 40% until Friday."}`, adminKey)
	if code != 200 || out["queued"] != float64(2) {
		t.Fatalf("send: %d %v (want 2: the paused partner is skipped)", code, out)
	}
	flushMail(t, h, sink)
	if len(mc.sent) != 2 {
		t.Fatalf("sent %d", len(mc.sent))
	}
	m := mc.sent[0]
	if m.To != "a@x.test" || m.Subject != "40% this month" || !strings.HasPrefix(m.Text, "Commission is 40% until Friday.") || m.ListUnsubscribe == "" {
		t.Fatalf("email: %+v", m)
	}
	if !strings.Contains(m.Text, m.ListUnsubscribe) {
		t.Fatalf("unsubscribe link missing from body:\n%s", m.Text)
	}

	// GET only asks; POST unsubscribes.
	res, err := http.Get(m.ListUnsubscribe)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var opt int
	h.DB.QueryRow(`SELECT news_opt_out FROM affiliates WHERE email = 'a@x.test'`).Scan(&opt)
	if res.StatusCode != 200 || opt != 0 {
		t.Fatalf("GET unsubscribed (or failed): %d opt=%d", res.StatusCode, opt)
	}
	tok := m.ListUnsubscribe[strings.Index(m.ListUnsubscribe, "t=")+2:]
	res, err = http.PostForm(srv.URL+"/api/v1/portal/unsubscribe", url.Values{"t": {tok}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	h.DB.QueryRow(`SELECT news_opt_out FROM affiliates WHERE email = 'a@x.test'`).Scan(&opt)
	if res.StatusCode != 200 || opt != 1 {
		t.Fatalf("POST unsubscribe: %d opt=%d", res.StatusCode, opt)
	}
	// A forged token for someone else does nothing.
	res, _ = http.PostForm(srv.URL+"/api/v1/portal/unsubscribe", url.Values{"t": {"2.deadbeef"}})
	res.Body.Close()
	h.DB.QueryRow(`SELECT news_opt_out FROM affiliates WHERE email = 'b@x.test'`).Scan(&opt)
	if res.StatusCode != http.StatusBadRequest || opt != 0 {
		t.Fatalf("forged token: %d opt=%d", res.StatusCode, opt)
	}
	// Next message skips the unsubscribed partner.
	_, out = postJSON(t, srv.URL+"/api/v1/admin/partners/message", `{"subject":"Next","body":"More news."}`, adminKey)
	if out["queued"] != float64(1) {
		t.Fatalf("after unsubscribe queued %v, want 1", out["queued"])
	}
	// Mail off: refused.
	srv2, _, key2 := portalEnv(t)
	if code, _ := postJSON(t, srv2.URL+"/api/v1/admin/partners/message", `{"subject":"a","body":"b"}`, key2); code != http.StatusServiceUnavailable {
		t.Fatalf("mail off: %d, want 503", code)
	}
}
