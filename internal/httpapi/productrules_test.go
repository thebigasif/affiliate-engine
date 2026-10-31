package httpapi_test

import (
	"testing"
)

// FR-12 per-product rates: admin rule CRUD round-trips product_key; bounds
// enforced; empty string stores NULL (product-agnostic semantics).
func TestAdmin_Rules_ProductKey(t *testing.T) {
	srv, h, key := adminEnv(t)
	h.DB.Exec(`INSERT INTO campaigns (id, name) VALUES (1, 'test')`)

	// Create with a product key.
	if code, out := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":3000,"product_key":"price_pro"}`, key); code != 201 {
		t.Fatalf("create with product_key: %d %v", code, out)
	}
	// Create a product-agnostic one (omitted key).
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":1000}`, key); code != 201 {
		t.Fatal("create without product_key rejected")
	}
	// Blank string = agnostic, not an error.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":900,"product_key":"  "}`, key); code != 201 {
		t.Fatal("blank product_key rejected")
	}
	// Over-length rejected.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":500,"product_key":"`+longKey(129)+`"}`, key); code != 400 {
		t.Fatal("129-char product_key accepted")
	}
	// 128 chars is the boundary: accepted.
	if code, _ := postJSON(t, srv.URL+"/api/v1/admin/rules",
		`{"campaign_id":1,"rate_type":"percent","rate_value":500,"product_key":"`+longKey(128)+`"}`, key); code != 201 {
		t.Fatal("128-char product_key rejected")
	}

	// List: keys present, blank for NULL.
	code, out := getJSON(t, srv.URL+"/api/v1/admin/rules?campaign_id=1", key)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	rules := out["rules"].([]any)
	if len(rules) != 4 {
		t.Fatalf("rules=%d want 4", len(rules))
	}
	seen := map[string]bool{}
	for _, r := range rules {
		m := r.(map[string]any)
		seen[m["product_key"].(string)] = true
	}
	if !seen["price_pro"] || !seen[""] {
		t.Fatalf("product_key round-trip: %v", seen)
	}
	// Trimming applied (the "  " create stored NULL -> "").
	var nulls int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commission_rules WHERE product_key IS NULL`).Scan(&nulls)
	if nulls != 2 {
		t.Fatalf("NULL product_key rows=%d want 2 (omitted + blank); 128-char boundary is a real key", nulls)
	}
}

func longKey(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'k'
	}
	return string(b)
}
