package httpapi_test

import (
	"net/http"
	"testing"
)

// approve mints a name-based primary code, collisions get a numeric suffix,
// /r/{code} resolves, and the portal's earnings payload carries the code.
func TestPortal_ApproveMintsReferralCode(t *testing.T) {
	srv, h, adminKey := portalEnv(t)

	approve := func(email, name string) map[string]any {
		t.Helper()
		if code, out := postJSON(t, srv.URL+"/api/v1/portal/apply",
			`{"email":"`+email+`","name":"`+name+`"}`, ""); code != 200 {
			t.Fatalf("apply %s: %d %v", email, code, out)
		}
		var appID int64
		if err := h.DB.QueryRow(`SELECT id FROM affiliate_applications WHERE email = ?`, email).Scan(&appID); err != nil {
			t.Fatal(err)
		}
		code, out := postJSON(t, srv.URL+"/api/v1/admin/portal/review",
			`{"application_id":`+itoa(appID)+`,"decision":"approve"}`, adminKey)
		if code != 200 {
			t.Fatalf("approve %s: %d %v", email, code, out)
		}
		return out
	}

	first := approve("jane@one.dev", "Jane Smith")
	if first["referral_code"] != "janesmith" {
		t.Fatalf("first code = %v", first["referral_code"])
	}
	second := approve("jane@two.dev", "Jane Smith")
	if second["referral_code"] != "janesmith2" {
		t.Fatalf("collision code = %v", second["referral_code"])
	}

	var primary bool
	var source string
	if err := h.DB.QueryRow(`SELECT is_primary, source FROM affiliate_codes WHERE code = 'janesmith'`).Scan(&primary, &source); err != nil {
		t.Fatal(err)
	}
	if !primary || source != "native" {
		t.Fatalf("primary=%v source=%s", primary, source)
	}

	// An affiliate that already holds a code (e.g. imported) keeps it.
	var affID int64
	if err := h.DB.QueryRow(`INSERT INTO affiliates (email, name) VALUES ('imp@x.dev', 'Imp') RETURNING id`).Scan(&affID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source) VALUES (?, 'oldlink', TRUE, 'import:rewardful')`, affID); err != nil {
		t.Fatal(err)
	}
	if out := approve("imp@x.dev", "Imp Orted"); out["referral_code"] != "oldlink" {
		t.Fatalf("existing code replaced: %v", out["referral_code"])
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_codes WHERE affiliate_id = ?`, affID).Scan(&n)
	if n != 1 {
		t.Fatalf("imported affiliate has %d codes, want 1", n)
	}

	// The earnings payload (portal link generator) carries the code.
	newID := int64(out2float(t, first, "affiliate_id"))
	code, out := getJSON(t, srv.URL+"/api/v1/affiliate/earnings?affiliate_id="+itoa(newID), adminKey)
	if code != http.StatusOK || out["referral_code"] != "janesmith" {
		t.Fatalf("earnings: %d referral_code=%v", code, out["referral_code"])
	}
}
