package httpapi_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

func mlmEnv(t *testing.T) (*httptest.Server, *db.Handles, string) {
	t.Helper()
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1,'a@t.dev'), (2,'b@t.dev'), (3,'c@t.dev')`)
	h.DB.Exec(`INSERT INTO mlm_tier_rates (campaign_id, level, rate_bp) VALUES (1, 1, 3000)`)
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('mx','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (3, 1, 10000, '{}', 'approved', ?)`, time.Now().UTC().Format(time.RFC3339))
	key := "ae_TestKeyMLMAdmin00000000000000000"
	h.DB.Exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKey', ?, 'admin', 'test')`, sha256hex(key))
	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{
		DB: h.DB, RateLimit: 1000, MLM: mlm.New(h.DB),
	}))
	t.Cleanup(srv.Close)
	return srv, h, key
}

// ML1: place sponsor + run period through the API.
func TestAdmin_MLMRun(t *testing.T) {
	srv, h, key := mlmEnv(t)
	// Place: 3 sponsored by 2, 2 sponsored by 1.
	if code, out := postJSON(t, srv.URL+"/api/v1/admin/mlm/place",
		`{"affiliate_id":2,"sponsor_id":1}`, key); code != 200 {
		t.Fatalf("place 2: %d %v", code, out)
	}
	if code, out := postJSON(t, srv.URL+"/api/v1/admin/mlm/place",
		`{"affiliate_id":3,"sponsor_id":2}`, key); code != 200 {
		t.Fatalf("place 3: %d %v", code, out)
	}
	// Cycle rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/mlm/place",
		`{"affiliate_id":1,"sponsor_id":3}`, key); code != 409 {
		t.Fatal("cycle accepted")
	}
	// Run the period covering the commission.
	now := time.Now().UTC()
	body := `{"start":"` + now.Add(-24*time.Hour).Format(time.RFC3339) + `","end":"` + now.Add(time.Hour).Format(time.RFC3339) + `"}`
	code, out := postJSON(t, srv.URL+"/api/v1/admin/mlm/run", body, key)
	if code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	if out["period_lines"].(float64) != 1 { // only L1 rate seeded
		t.Fatalf("lines: %v", out)
	}
	var paid int64
	h.DB.QueryRow(`SELECT SUM(commission_cents) FROM mlm_period_lines`).Scan(&paid)
	if paid != 3000 { // 30% of 10000
		t.Fatalf("tier total=%d want 3000", paid)
	}
	// Re-run refused (period paid).
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/mlm/run", body, key); code != 409 {
		t.Fatal("re-run accepted")
	}
}

// ML2: bad inputs.
func TestAdmin_MLMBadInput(t *testing.T) {
	srv, _, key := mlmEnv(t)
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/mlm/place", `{"affiliate_id":0}`, key); code != 400 {
		t.Fatal("no affiliate accepted")
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/mlm/place", `{"affiliate_id":9}`, key); code != 400 {
		t.Fatal("no parent/sponsor accepted")
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/mlm/run", `{"start":"x","end":"y"}`, key); code != 400 {
		t.Fatal("bad dates accepted")
	}
}
