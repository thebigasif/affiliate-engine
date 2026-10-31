package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// The site check finds the script, says when it's missing, and reports a
// page it couldn't open.
func TestSetupCheck_Site(t *testing.T) {
	f := setupAPI(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/with":
			w.Write([]byte(`<html><body>hi<script src="https://app.acme.test/t.js" async></script></body></html>`))
		case "/without":
			w.Write([]byte(`<html><body>no script</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	check := func(u string) map[string]any {
		t.Helper()
		code, m := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/setup-check/site", f.adminKey, `{"url":"`+u+`"}`)
		if code != 200 {
			t.Fatalf("%s: %d %v", u, code, m)
		}
		return m
	}
	if m := check(site.URL + "/with"); m["found"] != true || m["src"] != "https://app.acme.test/t.js" {
		t.Fatalf("with: %v", m)
	}
	if m := check(site.URL + "/without"); m["reached"] != true || m["found"] != false {
		t.Fatalf("without: %v", m)
	}
	if m := check(site.URL + "/gone"); m["reached"] != false || m["problem"] == nil {
		t.Fatalf("404: %v", m)
	}
	if code, _ := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/setup-check/site", f.adminKey, `{"url":"ftp://x"}`); code != 400 {
		t.Fatalf("bad url: %d", code)
	}
	// The real client never fetches internal addresses.
	if _, err := (&api{db: f.db.DB}).siteClient().Get(site.URL + "/with"); err == nil {
		t.Fatal("the default client fetched a loopback address")
	}
}

// Start makes one test partner with the setup-test link (again on a
// second run, active again), and the status reports its clicks and sales.
func TestSetupCheck_ClickAndSale(t *testing.T) {
	f := setupAPI(t)
	if dbtest.IsPostgres() { // the fixture inserts partners 1 and 2 by id
		f.db.DB.Exec(`SELECT setval(pg_get_serial_sequence('affiliates', 'id'), (SELECT MAX(id) FROM affiliates))`)
	}
	code, m := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/setup-check/start", f.adminKey, `{}`)
	if code != 200 || m["code"] != "setup-test" {
		t.Fatalf("start: %d %v", code, m)
	}
	id := int64(m["affiliate_id"].(float64))
	since := m["started_at"].(string)
	f.db.DB.Exec(`UPDATE affiliates SET status = 'frozen' WHERE id = ?`, id)
	if _, m2 := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/setup-check/start", f.adminKey, `{}`); int64(m2["affiliate_id"].(float64)) != id {
		t.Fatalf("second start made another partner: %v", m2)
	}
	var status string
	var optOut int
	f.db.DB.QueryRow(`SELECT status, news_opt_out FROM affiliates WHERE id = ?`, id).Scan(&status, &optOut)
	if status != "active" || optOut != 1 {
		t.Fatalf("test partner: %s opt_out=%d", status, optOut)
	}
	_, s := getJSON(t, f.srv.URL+"/api/v1/admin/setup-check?since="+since, f.adminKey)
	if s["clicks"] != float64(0) || len(s["sales"].([]any)) != 0 {
		t.Fatalf("before: %v", s)
	}
	ts := time.Now().UTC().Add(time.Second).Format(time.RFC3339)
	if _, err := f.db.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id) VALUES ('sc1', ?, 20261001, 202610, ?)`, ts, id); err != nil {
		t.Fatal(err)
	}
	_, s = getJSON(t, f.srv.URL+"/api/v1/admin/setup-check?since="+since, f.adminKey)
	if s["clicks"] != float64(1) {
		t.Fatalf("after click: %v", s)
	}
}
