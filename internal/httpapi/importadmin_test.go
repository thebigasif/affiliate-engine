package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// importCall sends one JSON request (body "" = GET) and decodes the reply.
func importCall(t *testing.T, url, body, key string) (int, map[string]any) {
	t.Helper()
	method := "GET"
	if body != "" {
		method = "POST"
	}
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

const importAffsCSV = `email,name,referral_id,status,coupon
jane@shop.io,Jane Doe,janeslink,active,JANE20
bob@shop.io,Bob,boblink,active,
`

const importRefsCSV = `affiliate_email,affiliate_name,customer_email,stripe_customer_id,coupon,amount,commission,status,created_at
jane@shop.io,Jane Doe,buyer1@x.test,cus_1,,49.00,14.70,paid,2026-03-02T09:00:00Z
`

// postImport sends the import form: fields plus files (name -> CSV).
func postImport(t *testing.T, f *apiFixture, key string, fields, files map[string]string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for k, v := range files {
		fw, _ := mw.CreateFormFile(k, k+".csv")
		_, _ = fw.Write([]byte(v))
	}
	_ = mw.Close()
	req, _ := http.NewRequest("POST", f.srv.URL+"/api/v1/admin/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// The admin screen's import: review writes nothing, import records a
// batch, the batch is listed, undo removes it (and is a no-op twice).
func TestAdminImport_ReviewImportListUndo(t *testing.T) {
	f := setupAPI(t)
	count := func() (n int) {
		f.db.DB.QueryRow(`SELECT COUNT(*) FROM affiliates`).Scan(&n)
		return n
	}
	before := count()
	files := map[string]string{"affiliates": importAffsCSV, "referrals": importRefsCSV}

	code, out := postImport(t, f, f.adminKey, map[string]string{"source": "rewardful", "dry_run": "1"}, files)
	if code != 200 {
		t.Fatalf("review: %d %v", code, out)
	}
	rep := out["report"].(map[string]any)
	if rep["dry_run"] != true || rep["affiliates"].(float64) != 2 || out["link_param"] != "via" {
		t.Fatalf("review report: %v", out)
	}
	if count() != before {
		t.Fatal("review wrote affiliates")
	}

	code, out = postImport(t, f, f.adminKey, map[string]string{"source": "rewardful"}, files)
	if code != 200 {
		t.Fatalf("import: %d %v", code, out)
	}
	rep = out["report"].(map[string]any)
	batch := rep["batch_id"].(float64)
	if batch <= 0 || count() != before+2 {
		t.Fatalf("import: batch=%v affiliates %d -> %d", batch, before, count())
	}

	code, out = importCall(t, f.srv.URL+"/api/v1/admin/import/batches", "", f.adminKey)
	if code != 200 || len(out["batches"].([]any)) != 1 {
		t.Fatalf("batches: %d %v", code, out)
	}

	body := fmt.Sprintf(`{"batch_id":%d}`, int64(batch))
	code, out = importCall(t, f.srv.URL+"/api/v1/admin/import/undo", body, f.adminKey)
	if code != 200 || count() != before {
		t.Fatalf("undo: %d %v, affiliates=%d want %d", code, out, count(), before)
	}
	code, out = importCall(t, f.srv.URL+"/api/v1/admin/import/undo", body, f.adminKey)
	if code != 200 || out["already_undone"] != true {
		t.Fatalf("second undo: %d %v", code, out)
	}
	if code, _ := importCall(t, f.srv.URL+"/api/v1/admin/import/undo", `{"batch_id":999}`, f.adminKey); code != 404 {
		t.Fatalf("unknown batch: %d", code)
	}
}

func TestAdminImport_BadInputs(t *testing.T) {
	f := setupAPI(t)
	for _, tc := range []struct {
		name   string
		key    string
		fields map[string]string
		files  map[string]string
		want   int
	}{
		{"no key", "", map[string]string{"source": "rewardful"}, map[string]string{"affiliates": importAffsCSV}, 401},
		{"affiliate key", f.aff1Key, map[string]string{"source": "rewardful"}, map[string]string{"affiliates": importAffsCSV}, 403},
		{"unknown source", f.adminKey, map[string]string{"source": "impact"}, map[string]string{"affiliates": importAffsCSV}, 400},
		{"no affiliates file", f.adminKey, map[string]string{"source": "rewardful"}, map[string]string{"referrals": importRefsCSV}, 400},
		{"not csv", f.adminKey, map[string]string{"source": "rewardful"}, map[string]string{"affiliates": "\x00\x01garbage"}, 400},
	} {
		if code, out := postImport(t, f, tc.key, tc.fields, tc.files); code != tc.want {
			t.Errorf("%s: %d %v, want %d", tc.name, code, out, tc.want)
		}
	}
}
