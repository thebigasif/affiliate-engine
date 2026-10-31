package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/httpapi"
)

// P4: a portal session (cookie) can read its own earnings — no API key.
func TestPortal_SessionReadsEarnings(t *testing.T) {
	h := dbtest.Open(t)
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (5, 'five@t.dev')`)
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('sj1','conversion', CURRENT_TIMESTAMP)`)
	h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (5, 1, 2500, '{}', 'approved')`)

	// Insert a session directly: token "aps_known", hash = sha256.
	tokHash := sha256hex("aps_knowntoken123")
	h.DB.Exec(`INSERT INTO affiliate_sessions (token_hash, affiliate_id, expires_at)
		VALUES (?, 5, '`+dbtest.At(1*24*time.Hour)+`')`, tokHash)

	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{DB: h.DB, RateLimit: 1000}))
	t.Cleanup(srv.Close)

	// Cookie auth.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/affiliate/earnings", nil)
	req.AddCookie(&http.Cookie{Name: "ae_portal_session", Value: "aps_knowntoken123"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session earnings: %d", resp.StatusCode)
	}

	// Bearer form too.
	req2, _ := http.NewRequest("GET", srv.URL+"/api/v1/affiliate/earnings", nil)
	req2.Header.Set("Authorization", "Bearer aps_knowntoken123")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("bearer session: %d", resp2.StatusCode)
	}

	// Revoked session rejected.
	h.DB.Exec(`UPDATE affiliate_sessions SET revoked_at = CURRENT_TIMESTAMP WHERE token_hash = ?`, tokHash)
	req3, _ := http.NewRequest("GET", srv.URL+"/api/v1/affiliate/earnings", nil)
	req3.AddCookie(&http.Cookie{Name: "ae_portal_session", Value: "aps_knowntoken123"})
	resp3, _ := http.DefaultClient.Do(req3)
	if resp3 == nil || resp3.StatusCode != 401 {
		if resp3 != nil {
			resp3.Body.Close()
		}
		t.Fatal("revoked session accepted")
	}
	resp3.Body.Close()

	// Session must NOT grant admin.
	req4, _ := http.NewRequest("GET", srv.URL+"/api/v1/admin/dashboard", nil)
	req4.AddCookie(&http.Cookie{Name: "ae_portal_session", Value: "aps_knowntoken123"})
	resp4, _ := http.DefaultClient.Do(req4)
	if resp4 == nil || resp4.StatusCode != 401 {
		t.Fatal("session used for admin route")
	}
	if resp4 != nil {
		resp4.Body.Close()
	}
	_ = strings.TrimSpace
	_ = time.Now
}
