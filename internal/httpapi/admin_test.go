package httpapi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
)

func sha256hexLocal(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func adminEnv(t *testing.T) (*httptest.Server, *db.Handles, string) {
	t.Helper()
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`)
	key := "ae_TestKeyAdminCRUD0000000000000000"
	h.DB.Exec(`INSERT INTO api_keys (prefix, key_hash, scope, name) VALUES ('ae_TestKey', ?, 'admin', 'test')`, sha256hexLocal(key))
	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{DB: h.DB, RateLimit: 1000}))
	t.Cleanup(srv.Close)
	return srv, h, key
}

// AD1: campaign create + list round-trip.
func TestAdmin_Campaigns(t *testing.T) {
	srv, _, key := adminEnv(t)
	code, out := postJSON(t, srv.URL+"/api/v1/admin/campaigns", `{"name":"Spring Launch"}`, key)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	code, out = getJSON(t, srv.URL+"/api/v1/admin/campaigns", key)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	camps := out["campaigns"].([]any)
	if len(camps) != 1 || camps[0].(map[string]any)["name"] != "Spring Launch" {
		t.Fatalf("campaigns: %v", camps)
	}
	// Empty name rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/campaigns", `{"name":"  "}`, key); code != 400 {
		t.Fatalf("blank name: %d", code)
	}
}

// AD2: rule validation — basis-point bounds, type checks.
func TestAdmin_Rules(t *testing.T) {
	srv, h, key := adminEnv(t)
	h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'test')`)
	// Valid percent rule.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":1000}`, key); code != 201 {
		t.Fatal("valid rule rejected")
	}
	// >100% rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":10001}`, key); code != 400 {
		t.Fatal("over-100% accepted")
	}
	// Negative rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"fixed","rate_value":-5}`, key); code != 400 {
		t.Fatal("negative accepted")
	}
	// Bad type rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"banana","rate_value":1}`, key); code != 400 {
		t.Fatal("banana type accepted")
	}
	// List filters by campaign.
	code, out := getJSON(t, srv.URL+"/api/v1/admin/rules?campaign_id=1", key)
	if code != 200 || len(out["rules"].([]any)) != 1 {
		t.Fatalf("rules list: %d %v", code, out)
	}
}

// AD3: affiliate postback_url update — shape-validated, stored.
func TestAdmin_AffiliateUpdate(t *testing.T) {
	srv, h, key := adminEnv(t)
	req, _ := http.NewRequest("PATCH", srv.URL+"/api/v1/admin/affiliates/1",
		strings.NewReader(`{"postback_url":"https://track.aff.dev/pb?cid={click_id}"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	var u string
	h.DB.QueryRow(`SELECT postback_url FROM affiliates WHERE id=1`).Scan(&u)
	if !strings.Contains(u, "track.aff.dev") {
		t.Fatalf("stored url: %q", u)
	}
	// Malformed rejected.
	req2, _ := http.NewRequest("PATCH", srv.URL+"/api/v1/admin/affiliates/1",
		strings.NewReader(`{"postback_url":"javascript:alert(1)"}`))
	req2.Header.Set("Authorization", "Bearer "+key)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("javascript: url accepted: %d", resp2.StatusCode)
	}
	// Bad id rejected.
	req3, _ := http.NewRequest("PATCH", srv.URL+"/api/v1/admin/affiliates/abc",
		strings.NewReader(`{}`))
	req3.Header.Set("Authorization", "Bearer "+key)
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != 400 {
		t.Fatal("bad id accepted")
	}
	_ = json.Marshal
}

// AD4: postback_secret lifecycle — minted once when a URL is first set,
// returned only in that response; re-setting the URL does NOT re-mint;
// {"rotate_secret":true} alone rotates to a fresh secret.
func TestAdmin_PostbackSecretLifecycle(t *testing.T) {
	srv, h, key := adminEnv(t)
	patch := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest("PATCH", srv.URL+"/api/v1/admin/affiliates/1",
			strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// 1. First URL set -> secret minted and returned once.
	code, out := patch(`{"postback_url":"https://track.aff.dev/pb"}`)
	if code != 200 {
		t.Fatalf("set url: %d", code)
	}
	secret1, _ := out["postback_secret"].(string)
	if len(secret1) != 48 { // ids.NewTokenHex(24)
		t.Fatalf("secret minted on first set: %q", secret1)
	}
	var stored string
	h.DB.QueryRow(`SELECT postback_secret FROM affiliates WHERE id=1`).Scan(&stored)
	if stored != secret1 {
		t.Fatalf("stored %q != returned %q", stored, secret1)
	}

	// 2. URL re-set without rotate -> no re-mint, no secret in response.
	code, out = patch(`{"postback_url":"https://track.aff.dev/pb2"}`)
	if code != 200 {
		t.Fatalf("re-set url: %d", code)
	}
	if s, ok := out["postback_secret"]; ok {
		t.Fatalf("re-set leaked a new secret %v (must not re-mint)", s)
	}
	h.DB.QueryRow(`SELECT postback_secret FROM affiliates WHERE id=1`).Scan(&stored)
	if stored != secret1 {
		t.Fatal("re-setting url rotated the secret")
	}

	// 3. rotate_secret alone (no url field) -> fresh secret, differs.
	code, out = patch(`{"rotate_secret":true}`)
	if code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	secret2, _ := out["postback_secret"].(string)
	if secret2 == "" || secret2 == secret1 {
		t.Fatalf("rotate returned %q, want fresh non-empty", secret2)
	}
	h.DB.QueryRow(`SELECT postback_secret FROM affiliates WHERE id=1`).Scan(&stored)
	if stored != secret2 {
		t.Fatalf("stored %q != rotated %q", stored, secret2)
	}

	// 4. Clearing the URL does not clear the secret (rotate is explicit).
	code, _ = patch(`{"postback_url":""}`)
	if code != 200 {
		t.Fatalf("clear url: %d", code)
	}
	h.DB.QueryRow(`SELECT postback_secret FROM affiliates WHERE id=1`).Scan(&stored)
	if stored != secret2 {
		t.Fatal("clearing url wiped the secret")
	}
}
