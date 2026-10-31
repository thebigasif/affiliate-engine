package httpapi_test

import (
	"fmt"
	"testing"
)

// FR-12 tiered rates: admin tier CRUD round-trips product_key +
// threshold_minor; bounds enforced; DELETE removes.
func TestAdmin_Tiers(t *testing.T) {
	srv, h, key := adminEnv(t)
	h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'test')`)

	// Create: product-agnostic tier.
	if code, out := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":1,"threshold_minor":100000,"rate_type":"percent","rate_value":2500}`, key); code != 201 {
		t.Fatalf("agnostic create: %d %v", code, out)
	}
	// Create: product-specific tier (empty string = agnostic; trim).
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":1,"product_key":" enterprise ","threshold_minor":0,"rate_type":"percent","rate_value":5000}`, key); code != 201 {
		t.Fatal("product create rejected")
	}
	// Missing threshold rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":1,"rate_type":"percent","rate_value":100}`, key); code != 400 {
		t.Fatal("missing threshold_minor accepted")
	}
	// Negative threshold rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":1,"threshold_minor":-1,"rate_type":"percent","rate_value":100}`, key); code != 400 {
		t.Fatal("negative threshold accepted")
	}
	// Over-100% rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":1,"threshold_minor":0,"rate_type":"percent","rate_value":10001}`, key); code != 400 {
		t.Fatal("over-100% accepted")
	}

	// List: both tiers present, key trimmed.
	code, out := getJSON(t, srv.URL+"/api/v1/admin/tiers?campaign_id=1", key)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	tiers := out["tiers"].([]any)
	if len(tiers) != 2 {
		t.Fatalf("tiers=%d want 2", len(tiers))
	}
	seen := map[string]bool{}
	for _, x := range tiers {
		m := x.(map[string]any)
		seen[m["product_key"].(string)] = true
		if m["currency"] != "USD" { // no currency given = the program currency
			t.Fatalf("tier currency=%v want USD", m["currency"])
		}
	}
	if !seen["enterprise"] || !seen[""] {
		t.Fatalf("product_key round-trip: %v", seen)
	}
	// A tier in another currency keeps it; a malformed code is refused.
	if code, out := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":2,"threshold_minor":1000000,"rate_type":"percent","rate_value":2000,"currency":"jpy"}`, key); code != 201 {
		t.Fatalf("JPY tier: %d %v", code, out)
	}
	if _, out := getJSON(t, srv.URL+"/api/v1/admin/tiers?campaign_id=2", key); out["tiers"].([]any)[0].(map[string]any)["currency"] != "JPY" {
		t.Fatalf("JPY tier round-trip: %v", out)
	}
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/tiers",
		`{"campaign_id":1,"threshold_minor":0,"rate_type":"percent","rate_value":100,"currency":"US$"}`, key); code != 400 {
		t.Fatal("malformed currency accepted")
	}

	// DELETE one: list shrinks; second delete of same id = 0 rows.
	var id float64
	for _, x := range tiers {
		m := x.(map[string]any)
		if m["product_key"].(string) == "enterprise" {
			id = m["id"].(float64)
		}
	}
	if code, out := deleteJSON(t, fmt.Sprintf("%s/api/v1/admin/tiers?id=%.0f", srv.URL, id), key); code != 200 {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, out = getJSON(t, srv.URL+"/api/v1/admin/tiers?campaign_id=1", key)
	if n := len(out["tiers"].([]any)); n != 1 {
		t.Fatalf("after delete tiers=%d want 1", n)
	}
	if code, out := deleteJSON(t, fmt.Sprintf("%s/api/v1/admin/tiers?id=%.0f", srv.URL, id), key); code != 200 {
		t.Fatalf("idempotent delete: %d %v", code, out)
	} else if d := out["deleted"].(float64); d != 0 {
		t.Fatalf("second delete removed %d rows, want 0", int(d))
	}
}
