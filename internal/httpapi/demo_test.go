package httpapi

// Live demo mode (AE_DEMO=1): reads with the public demo key, every write
// refused before it reaches a handler, real keys and sessions locked out.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// demoServer serves the setupAPI fixture's database in demo mode.
func demoServer(t *testing.T, f *apiFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewAPIHandler(APIDeps{
		DB:           f.db.DB,
		PayoutRunner: payouts.NewRunner(f.db.DB),
		Demo:         true,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestDemo_RefusesEveryWrite(t *testing.T) {
	f := setupAPI(t)
	srv := demoServer(t, f)
	count := func(table string) int {
		var n int
		if err := f.db.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tables := []string{
		"campaigns", "commission_rules", "payouts", "payout_runs", "commission_holds",
		"affiliate_applications", "affiliate_sessions", "tax_documents", "program_branding", "rate_tiers", "api_keys",
	}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = count(tb)
	}

	writes := []struct{ method, path, body string }{
		{"POST", "/api/v1/admin/payouts/run", `{"min_threshold_cents":0,"method":"manual"}`},
		{"POST", "/api/v1/admin/payouts/approve", `{"payout_id":1}`},
		{"POST", "/api/v1/admin/fraud/resolve", `{"commission_id":1,"decision":"uphold"}`},
		{"POST", "/api/v1/admin/campaigns", `{"name":"x"}`},
		{"POST", "/api/v1/admin/rules", `{"campaign_id":1,"rate_type":"percent","rate_value":100}`},
		{"POST", "/api/v1/admin/taxdocs", `{"affiliate_id":1,"doc_type":"w9","status":"pending"}`},
		{"POST", "/api/v1/admin/portal/review", `{"application_id":1,"decision":"approve"}`},
		{"PUT", "/api/v1/admin/branding?campaign_id=1", `{"brand_name":"pwned"}`},
		{"DELETE", "/api/v1/admin/tiers?id=1", ""},
		{"PATCH", "/api/v1/admin/affiliates/1", `{"postback_url":"https://evil.example/pb"}`},
		{"POST", "/api/v1/portal/apply", `{"email":"x@example.com"}`},
		{"POST", "/api/v1/portal/login", `{"grant":"aps_x"}`},
		{"POST", "/api/v1/portal/logout", ""},
		{"OPTIONS", "/api/v1/admin/dashboard", ""},
	}
	// Refused before auth: the demo key, a real admin key, or none.
	for _, key := range []string{DemoKey, f.adminKey, ""} {
		for _, w := range writes {
			code, m := do(t, w.method, srv.URL+w.path, key, w.body)
			if code != http.StatusForbidden || m["error"] != DemoReadOnlyMessage {
				t.Fatalf("%s %s (key %q): code=%d body=%v, want 403 read-only", w.method, w.path, key, code, m)
			}
		}
	}
	for _, tb := range tables {
		if n := count(tb); n != before[tb] {
			t.Fatalf("%s: %d rows, was %d — a demo write landed", tb, n, before[tb])
		}
	}
}

func TestDemo_ReadsWithDemoKey(t *testing.T) {
	f := setupAPI(t)
	srv := demoServer(t, f)
	for _, path := range []string{
		"/api/v1/admin/dashboard",
		"/api/v1/admin/affiliates",
		"/api/v1/admin/fraud/queue",
		"/api/v1/admin/campaigns",
		"/api/v1/admin/portal/applications",
		"/api/v1/affiliate/earnings?affiliate_id=1",
		"/api/v1/affiliate/ledger?affiliate_id=1",
		"/api/v1/portal/brand",
	} {
		code, m := do(t, "GET", srv.URL+path, DemoKey, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%v", path, code, m)
		}
	}
	// The ledger proof the portal shows holds over the demo key too.
	_, m := do(t, "GET", srv.URL+"/api/v1/affiliate/earnings?affiliate_id=1", DemoKey, "")
	if proof := m["ledger_proof"].(map[string]any); proof["holds"] != true {
		t.Fatalf("ledger proof: %v", proof)
	}
	if code, _ := do(t, "HEAD", srv.URL+"/api/v1/admin/dashboard", DemoKey, ""); code != http.StatusOK {
		t.Fatalf("HEAD dashboard: code=%d", code)
	}
}

func TestDemo_RejectsRealKeysAndSessions(t *testing.T) {
	f := setupAPI(t)
	srv := demoServer(t, f)
	// A portal session that exists in the database.
	tok := "aps_" + strings.Repeat("ab", 32)
	if _, err := f.db.DB.Exec(`INSERT INTO affiliate_sessions (token_hash, affiliate_id, expires_at) VALUES (?, 1, ?)`,
		hashToken(tok), time.Now().UTC().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, key string }{
		{"/api/v1/admin/dashboard", f.adminKey},
		{"/api/v1/admin/fraud/queue", f.adminKey},
		{"/api/v1/affiliate/earnings", f.aff1Key},
		{"/api/v1/affiliate/earnings", tok},
		{"/api/v1/affiliate/earnings", ""},
		{"/api/v1/admin/dashboard", "DEMO"},
		{"/api/v1/admin/dashboard", "demo-key"},
	} {
		if code, _ := do(t, "GET", srv.URL+c.path, c.key, ""); code != http.StatusUnauthorized {
			t.Fatalf("GET %s with %q: code=%d want 401", c.path, c.key, code)
		}
	}
	// The session cookie is ignored as well.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/affiliate/earnings", nil)
	req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: tok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session cookie: code=%d want 401", resp.StatusCode)
	}
}

// Outside demo mode nothing changes: "demo" is just a bad key, writes
// work, the brand payload carries no demo block.
func TestDemo_OffByDefault(t *testing.T) {
	f := setupAPI(t)
	if code, _ := do(t, "GET", f.srv.URL+"/api/v1/admin/dashboard", DemoKey, ""); code != http.StatusUnauthorized {
		t.Fatalf("demo key outside demo mode: code=%d want 401", code)
	}
	if code, m := do(t, "POST", f.srv.URL+"/api/v1/admin/campaigns", f.adminKey, `{"name":"Spring"}`); code != http.StatusCreated {
		t.Fatalf("create campaign: code=%d body=%v", code, m)
	}
	if _, m := do(t, "GET", f.srv.URL+"/api/v1/portal/brand", "", ""); m["demo"] != nil {
		t.Fatalf("brand payload has a demo block outside demo mode: %v", m)
	}
}

// The shell learns it is a demo from the brand payload it already loads:
// the public key, the trial link, and whose portal to show (top earner).
func TestDemo_BrandCarriesDemoInfo(t *testing.T) {
	f := setupAPI(t)
	srv := demoServer(t, f)
	code, m := do(t, "GET", srv.URL+"/api/v1/portal/brand", "", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	d, ok := m["demo"].(map[string]any)
	if !ok {
		t.Fatalf("no demo block: %v", m)
	}
	if d["key"] != DemoKey || d["trial_url"] != DemoTrialURL || d["affiliate_id"] != float64(1) {
		t.Fatalf("demo block: %v", d)
	}
	if m["accent_color"] == nil {
		t.Fatalf("branding fields lost: %v", m)
	}
}

// DemoGuard fronts the whole server: webhooks and MCP refused on every
// method, tracked links bounce to the dashboard without reaching the
// redirect handler, reads pass through.
func TestDemoGuard_FrontDoor(t *testing.T) {
	reached := ""
	g := DemoGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusTeapot)
	}))
	for _, c := range []struct {
		method, path string
		want         int
		passes       bool
	}{
		{"POST", "/webhooks/stripe", http.StatusForbidden, false},
		{"GET", "/webhooks/paddle", http.StatusForbidden, false},
		{"POST", "/mcp", http.StatusForbidden, false},
		{"GET", "/mcp", http.StatusForbidden, false},
		{"PUT", "/anything", http.StatusForbidden, false},
		{"GET", "/r/maya?dest=https://example.com/", http.StatusFound, false},
		{"GET", "/", http.StatusTeapot, true},
		{"HEAD", "/static/app.js", http.StatusTeapot, true},
		{"GET", "/healthz", http.StatusTeapot, true},
	} {
		reached = ""
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want || (reached != "") != c.passes {
			t.Fatalf("%s %s: code=%d reached=%q", c.method, c.path, rec.Code, reached)
		}
		if c.want == http.StatusFound && rec.Header().Get("Location") != "/" {
			t.Fatalf("/r/ redirect to %q, want /", rec.Header().Get("Location"))
		}
		if c.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), DemoReadOnlyMessage) {
			t.Fatalf("%s %s: body=%q", c.method, c.path, rec.Body.String())
		}
	}
}
