package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// Admin affiliates list pages by id: limit caps the page, next_after_id
// continues it, and the final page returns 0.
func TestAPI_AdminAffiliatesPagination(t *testing.T) {
	f := setupAPI(t) // affiliates 1 and 2
	code, m := getJSON(t, f.srv.URL+"/api/v1/admin/affiliates?limit=1", f.adminKey)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	affs := m["affiliates"].([]any)
	if len(affs) != 1 || affs[0].(map[string]any)["id"].(float64) != 1 {
		t.Fatalf("page1=%v", affs)
	}
	next := m["next_after_id"].(float64)
	if next != 1 {
		t.Fatalf("next_after_id=%v want 1", next)
	}
	_, m = getJSON(t, fmt.Sprintf("%s/api/v1/admin/affiliates?limit=1&after_id=%d", f.srv.URL, int64(next)), f.adminKey)
	affs = m["affiliates"].([]any)
	if len(affs) != 1 || affs[0].(map[string]any)["id"].(float64) != 2 {
		t.Fatalf("page2=%v", affs)
	}
	_, m = getJSON(t, f.srv.URL+"/api/v1/admin/affiliates?limit=1&after_id=2", f.adminKey)
	if n := len(m["affiliates"].([]any)); n != 0 || m["next_after_id"].(float64) != 0 {
		t.Fatalf("page3: n=%d next=%v want 0/0", n, m["next_after_id"])
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/affiliates?limit=abc", f.adminKey); code != http.StatusBadRequest {
		t.Fatalf("bad limit code=%d want 400", code)
	}
}

// Failed API-key attempts are cut off per client IP before the lookup.
func TestAPI_AuthFailureLimiter(t *testing.T) {
	f := setupAPI(t)
	last := 0
	for i := 0; i < authFailPerMin+2; i++ {
		last, _ = getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", "ae_wrong_key_guess")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d bad keys code=%d want 429", authFailPerMin+2, last)
	}
	// Even the right key is refused from that IP until the window rolls
	// (the check runs before the lookup).
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.adminKey); code != http.StatusTooManyRequests {
		t.Fatalf("valid key during lockout code=%d want 429", code)
	}
}
