package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

func portalEnv(t *testing.T) (*httptest.Server, *db.Handles, string) {
	t.Helper()
	return portalEnvWith(t, nil)
}

// portalEnvWith lets a test change the API deps (e.g. MailOn).
func portalEnvWith(t *testing.T, edit func(*httpapi.APIDeps)) (*httptest.Server, *db.Handles, string) {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
		"AFFILIATE_PAYABLE:2:USD": "liability", "PAYOUT_CLEARING:USD": "clearing",
	}); err != nil {
		t.Fatal(err)
	}
	// Seed a commission for the soon-to-be affiliate 2 so earnings are nonzero.
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('pj1','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (2, 1, 1500, '{}', 'approved')`)

	key := "ae_TestKeyPortalAuth00000000000000000"
	h.DB.Exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKey', ?, 'admin', 'test')`, sha256hex(key))

	deps := httpapi.APIDeps{DB: h.DB, RateLimit: 1000, LedgerPoster: poster}
	if edit != nil {
		edit(&deps)
	}
	srv := httptest.NewServer(httpapi.NewAPIHandler(deps))
	t.Cleanup(srv.Close)
	return srv, h, key
}

func postJSON(t *testing.T, url, body, auth string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, url, auth string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func deleteJSON(t *testing.T, url, auth string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", url, nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// P1: full loop — apply -> admin approves (grant minted) -> login with
// grant -> session reads own earnings.
func TestPortal_FullLoop(t *testing.T) {
	srv, h, adminKey := portalEnv(t)

	// 1) Apply (public).
	code, out := postJSON(t, srv.URL+"/api/v1/portal/apply",
		`{"email":"New@Partner.dev","name":"New Partner","note":"crypto blog"}`, "")
	if code != 200 || out["status"] != "received" {
		t.Fatalf("apply: %d %v", code, out)
	}
	// Duplicate apply: neutral.
	code, _ = postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"new@partner.dev"}`, "")
	if code != 200 {
		t.Fatalf("duplicate apply not neutral: %d", code)
	}

	// 2) Admin sees the queue.
	code, out = getJSON(t, srv.URL+"/api/v1/admin/portal/applications", adminKey)
	if code != 200 {
		t.Fatalf("queue: %d", code)
	}
	apps := out["applications"].([]any)
	if len(apps) != 1 {
		t.Fatalf("queue len=%d", len(apps))
	}
	appID := int64(apps[0].(map[string]any)["id"].(float64))

	// 3) Approve.
	code, out = postJSON(t, srv.URL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
	if code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	grant, _ := out["login_grant"].(string)
	if grant == "" {
		t.Fatal("no login grant minted")
	}

	// 4) Login with grant (public).
	code, out = postJSON(t, srv.URL+"/api/v1/portal/login",
		`{"grant":"`+grant+`"}`, "")
	if code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	session, _ := out["session"].(string)
	if session == "" {
		t.Fatal("no session issued")
	}

	// 5) Grant is one-time.
	code, _ = postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+grant+`"}`, "")
	if code != 401 {
		t.Fatalf("grant replay: %d want 401", code)
	}

	// 6) Session reads earnings — the seeded commission belongs to
	// affiliate 2; the new affiliate got the next id.
	newID := int64(out2float(t, out, "affiliate_id"))
	if newID == 0 {
		t.Fatal("no affiliate id in login response")
	}
	var email string
	h.DB.QueryRow(`SELECT email FROM affiliates WHERE id=?`, newID).Scan(&email)
	if !strings.EqualFold(email, "new@partner.dev") {
		t.Fatalf("affiliate email=%s", email)
	}
	t.Logf("new affiliate id=%d; session works", newID)
}

// P2: reject path + conflict on re-review.
func TestPortal_RejectAndConflict(t *testing.T) {
	srv, _, adminKey := portalEnv(t)
	postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"x@y.dev"}`, "")
	_, out := getJSON(t, srv.URL+"/api/v1/admin/portal/applications", adminKey)
	appID := int64(out["applications"].([]any)[0].(map[string]any)["id"].(float64))
	code, _ := postJSON(t, srv.URL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"reject"}`, adminKey)
	if code != 200 {
		t.Fatalf("reject: %d", code)
	}
	// Re-review -> conflict.
	code, _ = postJSON(t, srv.URL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
	if code != 409 {
		t.Fatalf("re-review: %d want 409", code)
	}
}

// With email on, a rejection queues the applicant's note with the reason.
func TestPortal_RejectEmailsReason(t *testing.T) {
	srv, h, adminKey := portalEnvWith(t, func(d *httpapi.APIDeps) { d.MailOn = true })
	postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"x@y.dev"}`, "")
	_, out := getJSON(t, srv.URL+"/api/v1/admin/portal/applications", adminKey)
	appID := int64(out["applications"].([]any)[0].(map[string]any)["id"].(float64))
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"reject","reason":"`+strings.Repeat("x", 1001)+`"}`, adminKey); code != 400 {
		t.Fatalf("long reason: %d", code)
	}
	code, m := postJSON(t, srv.URL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"reject","reason":" Newsletters only for now. "}`, adminKey)
	if code != 200 || m["emailed"] != true {
		t.Fatalf("reject: %d %v", code, m)
	}
	var payload string
	h.DB.QueryRow(`SELECT payload FROM outbox_events WHERE topic = 'mail.application_rejected'`).Scan(&payload)
	var queued struct{ Reason string }
	if json.Unmarshal([]byte(payload), &queued) != nil || queued.Reason != "Newsletters only for now." {
		t.Fatalf("queued: %q", payload)
	}
}

// P3: bad grants rejected, apply validation.
func TestPortal_BadInputs(t *testing.T) {
	srv, _, _ := portalEnv(t)
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"nope"}`, ""); code != 401 {
		t.Fatalf("bad grant: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"not-an-email"}`, ""); code != 400 {
		t.Fatalf("bad email: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/portal/review", `{"application_id":999,"decision":"approve"}`, "bad"); code != 401 {
		t.Fatalf("no admin key: %d", code)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func out2float(t *testing.T, m map[string]any, k string) float64 {
	t.Helper()
	v, ok := m[k].(float64)
	if !ok {
		return 0
	}
	return v
}

var _ = time.Now

// TestPortal_GrantDoubleSpend pins the concurrent double-spend guard: two
// racing logins with the SAME grant — exactly one session is issued.
func TestPortal_GrantDoubleSpend(t *testing.T) {
	srv, _, adminKey := portalEnv(t)
	// apply must run before the queue lookup; out unused here by design.
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/apply",
		`{"email":"race@partner.dev","name":"Race"}`, ""); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	code, out := getJSON(t, srv.URL+"/api/v1/admin/portal/applications", adminKey)
	if code != 200 {
		t.Fatalf("queue: %d", code)
	}
	appID := int64(out["applications"].([]any)[0].(map[string]any)["id"].(float64))
	code, out = postJSON(t, srv.URL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
	if code != 200 {
		t.Fatalf("approve: %d", code)
	}
	grant, _ := out["login_grant"].(string)

	type result struct {
		code int
		body map[string]any
	}
	resCh := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			c, b := postJSON(t, srv.URL+"/api/v1/portal/login",
				`{"grant":"`+grant+`"}`, "")
			resCh <- result{c, b}
		}()
	}
	wins, losses := 0, 0
	for i := 0; i < 2; i++ {
		r := <-resCh
		if r.code == 200 {
			wins++
		} else if r.code == 401 {
			losses++
		} else {
			t.Fatalf("unexpected status %d: %v", r.code, r.body)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("double-spend: wins=%d losses=%d (want 1/1)", wins, losses)
	}
}
