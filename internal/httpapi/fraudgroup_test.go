package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// FG1: releasing a hold of a review group releases the rest of the group,
// and the response names them.
func TestFraudResolve_ReleaseGroup(t *testing.T) {
	srv, h, _, key := fraudEnv(t)
	if _, err := h.DB.Exec(`UPDATE commission_holds SET hold_group = 'affiliate:1:commission:10'`); err != nil {
		t.Fatal(err)
	}
	body := `{"commission_id": 10, "decision": "release", "note": "reviewed"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/fraud/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		AlsoReleased []int64 `json:"also_released"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.AlsoReleased) != 1 || out.AlsoReleased[0] != 11 {
		t.Fatalf("also_released=%v want [11]", out.AlsoReleased)
	}
	var open int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("%d holds still open after a group release", open)
	}

	// The released member cannot be released again.
	req2, _ := http.NewRequest("POST", srv.URL+"/api/v1/admin/fraud/resolve",
		strings.NewReader(`{"commission_id": 11, "decision": "release"}`))
	req2.Header.Set("Authorization", "Bearer "+key)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second release status=%d want 409", resp2.StatusCode)
	}
}

// US2: usage commissions settled through the admin API are fraud-screened.
func TestAdmin_UsageSettleScreens(t *testing.T) {
	ctx := context.Background()
	h := dbtest.Open(t)
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'ai@t.dev')`,
		`INSERT INTO customers (program_id, external_customer_id, affiliate_id) VALUES (1, 'cus_api', 1)`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	key := "ae_TestKeyUsageScreen000000000000000"
	if _, err := h.DB.Exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKey', ?, 'admin', 'test')`, sha256hex(key)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{DB: h.DB, RateLimit: 1000, LedgerPoster: poster}))
	defer srv.Close()

	body := `{"events":[{"external_id":"s1","customer_id":"cus_api","units":50000,"unit_type":"tokens","occurred_at":"2026-09-15T10:00:00Z"}],
		"settle_day":20260915,"settle_rate_bp":300}`
	code, out := postJSON(t, srv.URL+"/api/v1/admin/usage", body, key)
	if code != 200 || out["settlements"].(float64) != 1 {
		t.Fatalf("settle: %d %v", code, out)
	}
	var screened bool
	if err := h.DB.QueryRow(`SELECT screened_at IS NOT NULL FROM commissions`).Scan(&screened); err != nil {
		t.Fatal(err)
	}
	if !screened {
		t.Fatal("usage commission settled through the API was not screened")
	}
}
