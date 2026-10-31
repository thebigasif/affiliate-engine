package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func putJSON(t *testing.T, url, key, body string) (int, map[string]any) {
	t.Helper()
	return sendJSON(t, "PUT", url, key, body)
}

func sendJSON(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// A partner's own rate: set, changed (the old one retired), shown in the
// partner list, and cleared back to the program rate. Partners can't.
func TestPartnerRate(t *testing.T) {
	f := setupAPI(t)
	url := f.srv.URL + "/api/v1/admin/affiliates/1/rate"
	for _, bad := range []string{`{"rate_type":"percent","rate_value":0}`, `{"rate_type":"percent","rate_value":10001}`, `{"rate_type":"bogus","rate_value":5}`} {
		if code, _ := putJSON(t, url, f.adminKey, bad); code != 400 {
			t.Fatalf("%s accepted: %d", bad, code)
		}
	}
	if code, _ := putJSON(t, f.srv.URL+"/api/v1/admin/affiliates/999/rate", f.adminKey, `{"rate_type":"percent","rate_value":4000}`); code != 404 {
		t.Fatalf("unknown partner: %d", code)
	}
	if code, _ := putJSON(t, url, f.aff1Key, `{"rate_type":"percent","rate_value":9000}`); code == 200 {
		t.Fatal("a partner set their own rate")
	}
	ownRate := func() any {
		t.Helper()
		_, m := getJSON(t, f.srv.URL+"/api/v1/admin/affiliates", f.adminKey)
		for _, x := range m["affiliates"].([]any) {
			if a := x.(map[string]any); a["id"].(float64) == 1 {
				return a["own_rate"]
			}
		}
		return "missing"
	}
	if r := ownRate(); r != nil {
		t.Fatalf("own rate before any: %v", r)
	}
	if code, m := putJSON(t, url, f.adminKey, `{"rate_type":"percent","rate_value":4000}`); code != 200 {
		t.Fatalf("set: %d %v", code, m)
	}
	if code, _ := putJSON(t, url, f.adminKey, `{"rate_type":"fixed","rate_value":500}`); code != 200 {
		t.Fatalf("change: %d", code)
	}
	if r, _ := ownRate().(map[string]any); r["rate_type"] != "fixed" || r["rate_value"] != float64(500) {
		t.Fatalf("own rate: %v", r)
	}
	var active int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM commission_rules WHERE affiliate_id = 1 AND active = TRUE`).Scan(&active)
	if active != 1 {
		t.Fatalf("%d active rules for partner 1, want 1", active)
	}
	if code, _ := putJSON(t, url, f.adminKey, `{"clear":true}`); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if r := ownRate(); r != nil {
		t.Fatalf("own rate after clear: %v", r)
	}
}

// The portal's deal: the bonus ladder and their progress; their own rate
// once they have one.
func TestPartnerDeal(t *testing.T) {
	f := setupAPI(t)
	if code, _ := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/tiers", f.adminKey, `{"campaign_id":1,"threshold_minor":500000,"rate_type":"percent","rate_value":3500}`); code != 201 {
		t.Fatalf("tier: %d", code)
	}
	_, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key)
	d, _ := m["deal"].(map[string]any)
	tiers, _ := d["tiers"].([]any)
	if d["own_rate"] != nil || len(tiers) != 1 || tiers[0].(map[string]any)["threshold_minor"] != float64(500000) {
		t.Fatalf("deal: %v", d)
	}
	if _, ok := d["volume_minor"].(float64); !ok {
		t.Fatalf("no volume: %v", d)
	}
	putJSON(t, f.srv.URL+"/api/v1/admin/affiliates/1/rate", f.adminKey, `{"rate_type":"percent","rate_value":4000}`)
	_, m = getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key)
	if r, _ := m["deal"].(map[string]any)["own_rate"].(map[string]any); r["rate_value"] != float64(4000) {
		t.Fatalf("own rate in deal: %v", m["deal"])
	}
}
