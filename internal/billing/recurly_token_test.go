package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/attribution"
	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// boundResolver is tokenResolver plus the customer-binding rung read from
// the customers table, like the real resolver.
type boundResolver struct{ db *sql.DB }

func (b boundResolver) Resolve(ctx context.Context, q attribution.Query) (attribution.Resolution, error) {
	var aff int64
	if q.CustomerExtID != "" && b.db.QueryRow(`SELECT affiliate_id FROM customers WHERE external_customer_id = ?`, q.CustomerExtID).Scan(&aff) == nil {
		return attribution.Resolution{AffiliateID: aff, Method: "customer", CampaignID: 1}, nil
	}
	return tokenResolver{}.Resolve(ctx, q)
}

func rxAcctXML(root, account, body string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<%s>
  <account><account_code>%s</account_code><email>%s@example.com</email></account>
  %s
</%s>`, root, account, account, body, root))
}

func rxPaidInvoice(uuid string, cents int64) string {
	return fmt.Sprintf(`<invoice><uuid>%s</uuid><state>collected</state><origin>purchase</origin>
    <invoice_number type="integer">7</invoice_number><total_in_cents type="integer">%d</total_in_cents>
    <currency>USD</currency><closed_at type="datetime">2026-09-30T10:00:00Z</closed_at></invoice>`, uuid, cents)
}

// The reader asks /accounts/code-<code> with the key as Basic Auth user,
// reads aff_token, and maps 404 to "" and 401/403 to ErrRecurlyAPIKey.
func TestRecurlyAccountToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "rk_ro" || pass != "" || r.Header.Get("Accept") == "" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.EscapedPath() {
		case "/accounts/code-acct%201":
			fmt.Fprint(w, `{"code":"acct 1","custom_fields":[{"name":"plan","value":"x"},{"name":"aff_token","value":" payload.sig "}]}`)
		case "/accounts/code-bare":
			fmt.Fprint(w, `{"code":"bare","custom_fields":[]}`)
		case "/accounts/code-boom":
			w.WriteHeader(503)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	read := billing.RecurlyAccountTokenAt("rk_ro", srv.URL)
	for code, want := range map[string]string{"acct 1": "payload.sig", "bare": "", "nobody": ""} {
		if got, err := read(ctx, code); err != nil || got != want {
			t.Fatalf("%q: %q, %v; want %q", code, got, err, want)
		}
	}
	if _, err := read(ctx, "boom"); err == nil || errors.Is(err, billing.ErrRecurlyAPIKey) {
		t.Fatalf("503: %v, want a transient error", err)
	}
	if _, err := billing.RecurlyAccountTokenAt("wrong", srv.URL)(ctx, "acct 1"); !errors.Is(err, billing.ErrRecurlyAPIKey) {
		t.Fatalf("bad key: %v", err)
	}
}

// The API key only ever goes to Recurly's own API hosts: any other
// AE_RECURLY_API_BASE is refused without a request (a refused key: the
// sale is ACKed and logged, not retried), and redirects aren't followed.
func TestRecurlyAccountTokenHostAllowlist(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"custom_fields":[{"name":"aff_token","value":"x"}]}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, base := range []string{srv.URL, "http://v3.recurly.com", "https://v3.recurly.com.evil.io", "https://169.254.169.254"} {
		if tok, err := billing.RecurlyAccountToken("rk_ro", base)(ctx, "acct"); !errors.Is(err, billing.ErrRecurlyAPIKey) || tok != "" {
			t.Fatalf("base %q: %q, %v", base, tok, err)
		}
	}
	if hits != 0 {
		t.Fatalf("%d requests to a host that isn't Recurly's", hits)
	}
}

// Real engine: a new account binds from its aff_token, its first paid
// invoice credits through the binding with no second API call, another
// account's first sale credits on the token rung, a transient API error
// makes Recurly retry, and a refused key ACKs the sale unattributed.
func TestRecurly_AffTokenRealMoney(t *testing.T) {
	eng, store, db := newRealMoney(t)
	calls := map[string]int{}
	var fail error
	a := billing.NewRecurlyAdapter("u", "p", "", eng, boundResolver{db}, store)
	a.AccountToken = func(ctx context.Context, code string) (string, error) {
		calls[code]++
		if fail != nil {
			return "", fail
		}
		return map[string]string{"trial": "payload.sig", "first": "at1_payload_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"}[code], nil
	}
	send := func(body []byte) error {
		return a.HandleWebhook(context.Background(), body, recurlyHeaders("u", "p"))
	}
	method := func(account string) (m string) {
		if err := db.QueryRow(`SELECT attribution_method FROM commissions WHERE customer_ext_id = ?`, account).Scan(&m); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return m
	}

	if err := send(rxAcctXML("new_account_notification", "trial", "")); err != nil {
		t.Fatal(err)
	}
	if err := send(rxAcctXML("closed_invoice_notification", "trial", rxPaidInvoice("inv-trial", 5000))); err != nil {
		t.Fatal(err)
	}
	if m := method("trial"); m != "customer" || calls["trial"] != 1 {
		t.Fatalf("trial: method=%q api calls=%d; want customer, 1", m, calls["trial"])
	}

	if err := send(rxAcctXML("closed_invoice_notification", "first", rxPaidInvoice("inv-first", 3000))); err != nil {
		t.Fatal(err)
	}
	if m := method("first"); m != "token" {
		t.Fatalf("first sale: method=%q; want token", m)
	}

	fail = errors.New("recurly API 503")
	if err := send(rxAcctXML("closed_invoice_notification", "later", rxPaidInvoice("inv-later", 3000))); err == nil {
		t.Fatal("transient API error ACKed; Recurly would not retry")
	}
	fail = billing.ErrRecurlyAPIKey
	if err := send(rxAcctXML("closed_invoice_notification", "refused", rxPaidInvoice("inv-refused", 3000))); err != nil {
		t.Fatalf("refused key must ACK (a retry cannot fix it): %v", err)
	}
	if m := method("refused"); m != "" {
		t.Fatalf("refused key credited %q", m)
	}
}
