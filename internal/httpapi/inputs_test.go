package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// Free-text and number bounds on admin writes: a value past the bound is a
// 400, never a stored row or a database error.
func TestAdmin_InputBounds(t *testing.T) {
	// The first case creates campaign 1 through the API (an explicit id
	// would leave the Postgres sequence behind); the rest use it.
	srv, _, key := adminEnv(t)
	send := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"campaign name at the limit", "POST", "/api/v1/admin/campaigns", `{"name":"` + strings.Repeat("é", 120) + `"}`, 201},
		{"campaign name too long", "POST", "/api/v1/admin/campaigns", `{"name":"` + strings.Repeat("a", 121) + `"}`, 400},
		{"fixed rule at the cap", "POST", "/api/v1/admin/rules", `{"campaign_id":1,"rate_type":"fixed","rate_value":100000000000}`, 201},
		{"fixed rule past the cap", "POST", "/api/v1/admin/rules", `{"campaign_id":1,"rate_type":"fixed","rate_value":9223372036854775807}`, 400},
		{"rule priority in range", "POST", "/api/v1/admin/rules", `{"campaign_id":1,"rate_type":"percent","rate_value":100,"priority":-1000000}`, 201},
		{"rule priority past a 32-bit column", "POST", "/api/v1/admin/rules", `{"campaign_id":1,"rate_type":"percent","rate_value":100,"priority":3000000000}`, 400},
		{"fixed tier past the cap", "POST", "/api/v1/admin/tiers", `{"campaign_id":1,"threshold_minor":0,"rate_type":"fixed","rate_value":100000000001}`, 400},
		{"partner fixed rate past the cap", "PUT", "/api/v1/admin/affiliates/1/rate", `{"campaign_id":1,"rate_type":"fixed","rate_value":100000000001}`, 400},
		{"fraud note too long", "POST", "/api/v1/admin/fraud/resolve", `{"commission_id":1,"decision":"release","note":"` + strings.Repeat("n", 1001) + `"}`, 400},
		{"fraud note fine, no hold", "POST", "/api/v1/admin/fraud/resolve", `{"commission_id":1,"decision":"release","note":"ok"}`, 404},
	}
	for _, c := range cases {
		if got := send(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// A long name or note on the public signup form is cut on a character
// boundary, so the row stays valid UTF-8 (Postgres refused the old cut).
func TestPortalApply_TrimsOnCharacterBoundary(t *testing.T) {
	srv, h, _ := portalEnv(t)
	name := "a" + strings.Repeat("é", 70) // 141 bytes: a cut at 120 lands inside an é
	note := "a" + strings.Repeat("é", 300)
	code, out := postJSON(t, srv.URL+"/api/v1/portal/apply", `{"email":"long@x.test","name":"`+name+`","note":"`+note+`"}`, "")
	if code != 200 || out["status"] != "received" {
		t.Fatalf("apply: %d %v", code, out)
	}
	var gotName, gotNote string
	if err := h.DB.QueryRow(`SELECT name, note FROM affiliate_applications WHERE email = 'long@x.test'`).Scan(&gotName, &gotNote); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(gotName) || !utf8.ValidString(gotNote) || len(gotName) > 120 || len(gotNote) > 500 || !strings.HasPrefix(name, gotName) {
		t.Fatalf("stored name=%q (%d bytes) note %d bytes", gotName, len(gotName), len(gotNote))
	}
}
