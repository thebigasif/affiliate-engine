package httpapi_test

// Portal hardening pins: grants only from the POST body, frozen affiliates
// lose login + live sessions, logout kills the session.

import (
	"net/http"
	"strings"
	"testing"
)

// approveNewAffiliate runs apply -> admin approve and returns the grant.
func approveNewAffiliate(t *testing.T, srvURL, adminKey, email string) string {
	t.Helper()
	if code, _ := postJSON(t, srvURL+"/api/v1/portal/apply", `{"email":"`+email+`"}`, ""); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	_, out := getJSON(t, srvURL+"/api/v1/admin/portal/applications", adminKey)
	var appID int64
	for _, a := range out["applications"].([]any) {
		m := a.(map[string]any)
		if m["email"] == email {
			appID = int64(m["id"].(float64))
		}
	}
	code, out := postJSON(t, srvURL+"/api/v1/admin/portal/review",
		`{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
	if code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	grant, _ := out["login_grant"].(string)
	if grant == "" {
		t.Fatal("no grant")
	}
	return grant
}

// A grant in the query string is never accepted (it would land in logs).
func TestPortal_GrantQueryStringRejected(t *testing.T) {
	srv, _, adminKey := portalEnv(t)
	grant := approveNewAffiliate(t, srv.URL, adminKey, "q@partner.dev")
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/login?grant="+grant, ``, ""); code != 400 {
		t.Fatalf("query-string grant: %d want 400", code)
	}
	// The grant was not consumed: the body form still works.
	if code, out := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+grant+`"}`, ""); code != 200 {
		t.Fatalf("body grant: %d %v", code, out)
	}
}

// Frozen affiliates cannot log in, and their live sessions stop working.
func TestPortal_FrozenAffiliateLosesAccess(t *testing.T) {
	srv, h, adminKey := portalEnv(t)
	grant := approveNewAffiliate(t, srv.URL, adminKey, "frz@partner.dev")
	code, out := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+grant+`"}`, "")
	if code != 200 {
		t.Fatalf("login: %d", code)
	}
	session, _ := out["session"].(string)
	if code, _ := getJSON(t, srv.URL+"/api/v1/affiliate/earnings", session); code != 200 {
		t.Fatalf("active session: %d", code)
	}
	if _, err := h.DB.Exec(`UPDATE affiliates SET status = 'frozen' WHERE email = 'frz@partner.dev'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := getJSON(t, srv.URL+"/api/v1/affiliate/earnings", session); code != 401 {
		t.Fatalf("frozen affiliate session: %d want 401", code)
	}

	// A fresh grant for a frozen affiliate is refused too.
	grant2 := approveNewAffiliate(t, srv.URL, adminKey, "frz2@partner.dev")
	if _, err := h.DB.Exec(`UPDATE affiliates SET status = 'frozen' WHERE email = 'frz2@partner.dev'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+grant2+`"}`, ""); code != 401 {
		t.Fatalf("frozen login: %d want 401", code)
	}
}

// Logout deletes the session server-side and expires the cookie.
func TestPortal_Logout(t *testing.T) {
	srv, h, adminKey := portalEnv(t)
	grant := approveNewAffiliate(t, srv.URL, adminKey, "out@partner.dev")
	_, out := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+grant+`"}`, "")
	session, _ := out["session"].(string)

	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/portal/logout", strings.NewReader(""))
	req.AddCookie(&http.Cookie{Name: "ae_portal_session", Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == "ae_portal_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not expire the session cookie")
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_sessions WHERE token_hash = ?`, sha256hex(session)).Scan(&n)
	if n != 0 {
		t.Fatalf("session row survived logout: %d", n)
	}
	if code, _ := getJSON(t, srv.URL+"/api/v1/affiliate/earnings", session); code != 401 {
		t.Fatalf("logged-out session: %d want 401", code)
	}
	// Idempotent: logging out with no session is fine.
	if code, _ := postJSON(t, srv.URL+"/api/v1/portal/logout", ``, ""); code != 200 {
		t.Fatalf("anonymous logout: %d", code)
	}
}
