package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/httpapi"
)

// GET /admin/settings keeps its fields and adds setup: the billing webhooks
// that are on and whether partner email is on.
func TestSettings_SetupStatus(t *testing.T) {
	srv, _, key := portalEnvWith(t, func(d *httpapi.APIDeps) { d.Billing = []string{"stripe"}; d.MailOn = true })
	code, out := getJSON(t, srv.URL+"/api/v1/admin/settings", key)
	if code != 200 || out["min_payout_cents"] != float64(5000) {
		t.Fatalf("settings: %d %v", code, out)
	}
	setup, _ := out["setup"].(map[string]any)
	if b, _ := setup["billing"].([]any); len(b) != 1 || b[0] != "stripe" || setup["mail"] != true {
		t.Fatalf("setup: %v", out["setup"])
	}

	srv2, _, key2 := portalEnv(t)
	_, out = getJSON(t, srv2.URL+"/api/v1/admin/settings", key2)
	setup, _ = out["setup"].(map[string]any)
	if b, ok := setup["billing"].([]any); !ok || len(b) != 0 || setup["mail"] != false {
		t.Fatalf("setup with nothing on: %v", out["setup"])
	}
}

// setup.webhooks reports each billing tool's last accepted and rejected
// event, and its failed signature checks apart.
func TestSettings_WebhookHealth(t *testing.T) {
	srv, h, key := portalEnv(t)
	if _, err := h.DB.Exec(`INSERT INTO webhook_health (provider, last_ok_at, ok_count, last_err_at, last_err, err_count, last_sigfail_at, sigfail_count)
		VALUES ('stripe', '2026-09-30T10:00:00Z', 5, '2026-09-30T11:00:00Z', 'store: database is locked', 1, '2026-09-30T12:00:00Z', 7)`); err != nil {
		t.Fatal(err)
	}
	_, out := getJSON(t, srv.URL+"/api/v1/admin/settings", key)
	setup, _ := out["setup"].(map[string]any)
	wh, _ := setup["webhooks"].([]any)
	if len(wh) != 1 {
		t.Fatalf("webhooks: %v", setup["webhooks"])
	}
	w := wh[0].(map[string]any)
	if w["provider"] != "stripe" || w["ok_count"] != float64(5) || w["err_count"] != float64(1) || w["last_err"] != "store: database is locked" ||
		w["last_sigfail_at"] != "2026-09-30T12:00:00Z" || w["sigfail_count"] != float64(7) {
		t.Fatalf("stripe health: %v", w)
	}
}

// The public brand payload carries the program's offer once it has a
// default commission rule, for the partner signup page.
func TestPortalBrand_Terms(t *testing.T) {
	srv, h, _ := portalEnv(t)
	_, out := getJSON(t, srv.URL+"/api/v1/portal/brand", "")
	if _, ok := out["terms"]; ok {
		t.Fatalf("terms before any rule: %v", out["terms"])
	}
	h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'Main program')`)
	h.DB.Exec(`UPDATE campaigns SET commission_months = 12, attribution_window_days = 60 WHERE id = 1`)
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, rate_type, rate_value, priority) VALUES (1, 'percent', 3000, 1)`); err != nil {
		t.Fatal(err)
	}
	_, out = getJSON(t, srv.URL+"/api/v1/portal/brand", "")
	terms, _ := out["terms"].(map[string]any)
	if terms["rate_type"] != "percent" || terms["rate_value"] != float64(3000) || terms["commission_months"] != float64(12) || terms["cookie_days"] != float64(60) || terms["min_payout_cents"] != float64(5000) {
		t.Fatalf("terms: %v", out["terms"])
	}
}

// Pausing a partner cuts their sessions; resuming restores access. Only
// active and frozen are accepted, and a removed partner stays removed.
func TestAffiliate_PauseResume(t *testing.T) {
	srv, h, key := portalEnv(t)
	var id int64
	if err := h.DB.QueryRow(`INSERT INTO affiliates (email, name) VALUES ('p@x.test', 'P') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("PATCH", srv.URL+"/api/v1/admin/affiliates/"+itoa(id), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	statusOf := func() (s string) { h.DB.QueryRow(`SELECT status FROM affiliates WHERE id = ?`, id).Scan(&s); return }

	if code, out := patch(`{"status":"frozen"}`); code != 200 || out["status"] != "frozen" || statusOf() != "frozen" {
		t.Fatalf("pause: %d %v (db %s)", code, out, statusOf())
	}
	if code, _ := patch(`{"status":"active"}`); code != 200 || statusOf() != "active" {
		t.Fatalf("resume: %d (db %s)", code, statusOf())
	}
	if code, _ := patch(`{"status":"removed"}`); code != http.StatusBadRequest {
		t.Fatalf("status removed accepted: %d", code)
	}
	h.DB.Exec(`UPDATE affiliates SET status = 'removed' WHERE id = ?`, id)
	if code, _ := patch(`{"status":"active"}`); code != http.StatusNotFound || statusOf() != "removed" {
		t.Fatalf("revived a removed partner: %d (db %s)", code, statusOf())
	}
}

// Settings carries the owner's notification email: on by default to the
// install's owner address, changeable, and validated.
func TestSettings_OwnerNotify(t *testing.T) {
	srv, _, key := portalEnvWith(t, func(d *httpapi.APIDeps) { d.MailOn = true; d.OwnerEmail = "owner@acme.test" })
	get := func() map[string]any {
		t.Helper()
		_, out := getJSON(t, srv.URL+"/api/v1/admin/settings", key)
		n, _ := out["notify"].(map[string]any)
		return n
	}
	if n := get(); n["email"] != "owner@acme.test" || n["enabled"] != true {
		t.Fatalf("default notify: %v", n)
	}
	put := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/admin/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(`{"notify_email":"not an email"}`); code != 400 {
		t.Fatalf("bad email: %d", code)
	}
	if code := put(`{"notify_on":true,"notify_email":"boss@acme.test"}`); code != 200 {
		t.Fatalf("save: %d", code)
	}
	if n := get(); n["email"] != "boss@acme.test" || n["enabled"] != true {
		t.Fatalf("after save: %v", n)
	}
	if code := put(`{"notify_on":false}`); code != 200 {
		t.Fatalf("turn off: %d", code)
	}
	if n := get(); n["enabled"] != false || n["email"] != "boss@acme.test" {
		t.Fatalf("after off: %v", n)
	}
	// The regular emails: on by default, each switched on its own.
	if n := get(); n["weekly_report"] != true || n["partner_tips"] != true || n["partner_recap"] != true {
		t.Fatalf("default switches: %v", n)
	}
	if code := put(`{"weekly_report":false,"partner_recap":false}`); code != 200 {
		t.Fatalf("switches: %d", code)
	}
	if n := get(); n["weekly_report"] != false || n["partner_tips"] != true || n["partner_recap"] != false || n["enabled"] != false {
		t.Fatalf("after switches: %v", n)
	}
	if code := put(`{"weekly_report":true,"notify_email":""}`); code != 400 {
		t.Fatalf("weekly report without an address: %d", code)
	}
	// Payout settings still save on their own.
	if code := put(`{"min_payout_cents":7000}`); code != 200 {
		t.Fatalf("payout settings: %d", code)
	}
	if _, out := getJSON(t, srv.URL+"/api/v1/admin/settings", key); out["min_payout_cents"] != float64(7000) {
		t.Fatalf("min payout not saved: %v", out)
	}
}
