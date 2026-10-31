package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// C1: code CRUD — validation, normalization, conflict, list, delete, auth.
func TestCodes_CRUD(t *testing.T) {
	srv, h, key := adminEnv(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'b@t.dev')`); err != nil {
		t.Fatal(err)
	}

	// Invalid: too short / bad chars / unknown affiliate.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":1,"code":"x"}`, key); code != 400 {
		t.Fatalf("short code accepted: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":1,"code":"bad code!"}`, key); code != 400 {
		t.Fatalf("bad chars accepted: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":99,"code":"ghost"}`, key); code != 400 {
		t.Fatalf("unknown affiliate accepted: %d", code)
	}

	// Create (uppercase normalizes to lowercase).
	if code, out := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":1,"code":"Asif","primary":true}`, key); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	// Conflict: same code for another affiliate.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":2,"code":"asif"}`, key); code != 409 {
		t.Fatalf("duplicate code accepted: %d", code)
	}

	// List filtered by affiliate shows normalized code + primary flag.
	code, out := getJSON(t, srv.URL+"/api/v1/admin/codes?affiliate_id=1", key)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"code":"asif"`) {
		t.Fatalf("list missing code: %s", b)
	}

	// Promote a second code: the first loses primary.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":1,"code":"asif2","primary":true}`, key); code != 201 {
		t.Fatalf("create 2: %d", code)
	}
	var primaries int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_codes WHERE affiliate_id=1 AND is_primary=TRUE`).Scan(&primaries)
	if primaries != 1 {
		t.Fatalf("primaries=%d, want 1", primaries)
	}

	// DELETE removes the code.
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/v1/admin/codes?code=asif2", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_codes WHERE code='asif2'`).Scan(&n)
	if n != 0 {
		t.Fatalf("code not deleted")
	}

	// Auth enforced.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/codes", `{"affiliate_id":1,"code":"zzz"}`, "bad-key"); code != 401 {
		t.Fatalf("no-key create accepted: %d", code)
	}
}
