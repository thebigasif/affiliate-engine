package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testPDF = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\nW-9 SSN 123-45-6789\n%%EOF\n")

func uploadTaxDoc(t *testing.T, url, key, docType, name string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if docType != "" {
		_ = mw.WriteField("doc_type", docType)
	}
	if data != nil {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(data)
	}
	_ = mw.Close()
	req, _ := http.NewRequest("POST", url, &buf)
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

func rawGet(t *testing.T, url, key string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func adminSetTaxDoc(t *testing.T, f *apiFixture, aff int64, docType, status string) {
	t.Helper()
	code, out := paPost(t, f.srv.URL+"/api/v1/admin/taxdocs",
		fmt.Sprintf(`{"affiliate_id":%d,"doc_type":%q,"status":%q}`, aff, docType, status), f.adminKey)
	if code != http.StatusCreated {
		t.Fatalf("admin set %s: %d %v", status, code, out)
	}
}

// Portal flow: affiliate uploads a W-9, status becomes 'pending', admin
// sees the file and downloads the exact bytes as an attachment; nobody
// else can read it.
func TestTaxDocUpload_PortalFlow(t *testing.T) {
	f := setupAPI(t)
	code, out := uploadTaxDoc(t, f.srv.URL+"/api/v1/affiliate/taxdocs", f.aff1Key, "w9", "../../etc/My W9.pdf", testPDF)
	if code != http.StatusCreated || out["status"] != "pending" {
		t.Fatalf("upload: %d %v", code, out)
	}
	docID := int64(out["id"].(float64))

	// Affiliate sees their own status and file metadata, never the bytes.
	code, out = getJSON(t, f.srv.URL+"/api/v1/affiliate/taxdocs", f.aff1Key)
	docs := out["tax_documents"].([]any)
	if code != 200 || len(docs) != 1 {
		t.Fatalf("own list: %d %v", code, out)
	}
	d := docs[0].(map[string]any)
	if d["status"] != "pending" || d["file_name"] != "My W9.pdf" || d["uploaded_at"] == "" {
		t.Fatalf("own doc: %v", d)
	}
	// Another affiliate sees nothing of it.
	_, out = getJSON(t, f.srv.URL+"/api/v1/affiliate/taxdocs", f.aff2Key)
	if n := len(out["tax_documents"].([]any)); n != 0 {
		t.Fatalf("aff2 sees %d docs", n)
	}

	// Admin list shows the file; admin download returns the exact bytes.
	_, out = getJSON(t, f.srv.URL+"/api/v1/admin/taxdocs?affiliate_id=1", f.adminKey)
	ad := out["tax_documents"].([]any)[0].(map[string]any)
	if ad["has_file"] != true || ad["reviewed_at"] != "" {
		t.Fatalf("admin doc: %v", ad)
	}
	res, body := rawGet(t, fmt.Sprintf("%s/api/v1/admin/taxdocs/file?id=%d", f.srv.URL, docID), f.adminKey)
	if res.StatusCode != 200 || !bytes.Equal(body, testPDF) {
		t.Fatalf("download: %d %q", res.StatusCode, body)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition=%q", cd)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download headers: %v", res.Header)
	}
	// Affiliate keys cannot use the admin download (not even their own).
	res, _ = rawGet(t, fmt.Sprintf("%s/api/v1/admin/taxdocs/file?id=%d", f.srv.URL, docID), f.aff1Key)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("affiliate download: %d want 403", res.StatusCode)
	}

	// Sealed at rest: the stored blob is not the plaintext.
	var sealed []byte
	var size int64
	if err := f.db.DB.QueryRow(`SELECT sealed, size_bytes FROM tax_document_files WHERE tax_document_id = ?`, docID).
		Scan(&sealed, &size); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("%PDF")) || bytes.Contains(sealed, []byte("123-45-6789")) || size != int64(len(testPDF)) {
		t.Fatalf("file stored in the clear (size=%d)", size)
	}

	// Admin validates; a re-upload resets to 'pending' and replaces the file.
	adminSetTaxDoc(t, f, 1, "w9", "validated")
	pdf2 := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 100)...)
	if code, out := uploadTaxDoc(t, f.srv.URL+"/api/v1/affiliate/taxdocs", f.aff1Key, "w9", "v2.pdf", pdf2); code != 201 {
		t.Fatalf("re-upload: %d %v", code, out)
	}
	var st string
	var nFiles int
	f.db.DB.QueryRow(`SELECT status FROM tax_documents WHERE id = ?`, docID).Scan(&st)
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM tax_document_files WHERE affiliate_id = 1`).Scan(&nFiles)
	if st != "pending" || nFiles != 1 {
		t.Fatalf("re-upload: status=%s files=%d", st, nFiles)
	}
	_, body = rawGet(t, fmt.Sprintf("%s/api/v1/admin/taxdocs/file?id=%d", f.srv.URL, docID), f.adminKey)
	if !bytes.Equal(body, pdf2) {
		t.Fatal("download after re-upload is not the new file")
	}
	// Status transitions are audited (existing trigger).
	var audits int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE table_name = 'tax_documents' AND row_id = ?`, docID).Scan(&audits)
	if audits < 2 {
		t.Fatalf("audit rows=%d want >= 2", audits)
	}
}

func TestTaxDocUpload_Rejects(t *testing.T) {
	f := setupAPI(t)
	url := f.srv.URL + "/api/v1/affiliate/taxdocs"
	cases := []struct {
		name, docType string
		data          []byte
		want          int
	}{
		{"not a pdf", "w9", []byte("<html><script>alert(1)</script>"), http.StatusUnsupportedMediaType},
		{"pdf magic not at start", "w9", append([]byte(" "), testPDF...), http.StatusUnsupportedMediaType},
		{"too large", "w9", append([]byte("%PDF-1.4\n"), make([]byte, MaxTaxDocBytes)...), http.StatusRequestEntityTooLarge},
		{"bad type", "w2", testPDF, http.StatusBadRequest},
		{"admin-only type", "other", testPDF, http.StatusBadRequest},
		{"no type", "", testPDF, http.StatusBadRequest},
		{"no file", "w9", nil, http.StatusBadRequest},
		{"empty file", "w9", []byte{}, http.StatusBadRequest},
	}
	for _, c := range cases {
		code, out := uploadTaxDoc(t, url, f.aff1Key, c.docType, "f.pdf", c.data)
		if code != c.want {
			t.Errorf("%s: %d %v want %d", c.name, code, out, c.want)
		}
	}
	var n int
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM tax_documents`).Scan(&n)
	if n != 0 {
		t.Fatalf("rejected uploads created %d tax_documents rows", n)
	}
	// Unauthenticated: refused.
	if code, _ := uploadTaxDoc(t, url, "", "w9", "f.pdf", testPDF); code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload: %d want 401", code)
	}
	// Not JSON / not multipart.
	code, _ := paPost(t, url, `{"doc_type":"w9"}`, f.aff1Key)
	if code != http.StatusBadRequest {
		t.Fatalf("json body: %d want 400", code)
	}
}

// No sealing key configured: uploads are refused, never stored in the clear.
func TestTaxDocUpload_NoKeyRefused(t *testing.T) {
	f := setupAPI(t)
	srv := httptest.NewServer(NewAPIHandler(APIDeps{DB: f.db.DB}))
	defer srv.Close()
	code, _ := uploadTaxDoc(t, srv.URL+"/api/v1/affiliate/taxdocs", f.aff1Key, "w9", "f.pdf", testPDF)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("upload without key: %d want 503", code)
	}
}

// A sealed blob is bound to its row: copied onto another affiliate's
// document it does not decrypt.
func TestTaxDocFile_BoundToRow(t *testing.T) {
	f := setupAPI(t)
	url := f.srv.URL + "/api/v1/affiliate/taxdocs"
	_, o1 := uploadTaxDoc(t, url, f.aff1Key, "w9", "a.pdf", testPDF)
	_, o2 := uploadTaxDoc(t, url, f.aff2Key, "w8ben", "b.pdf", []byte("%PDF-1.4 other"))
	id1, id2 := int64(o1["id"].(float64)), int64(o2["id"].(float64))
	if _, err := f.db.DB.Exec(`UPDATE tax_document_files SET sealed = (SELECT sealed FROM tax_document_files WHERE tax_document_id = ?)
		WHERE tax_document_id = ?`, id1, id2); err != nil {
		t.Fatal(err)
	}
	res, body := rawGet(t, fmt.Sprintf("%s/api/v1/admin/taxdocs/file?id=%d", f.srv.URL, id2), f.adminKey)
	if res.StatusCode != http.StatusInternalServerError || bytes.Contains(body, []byte("123-45-6789")) {
		t.Fatalf("swapped blob decrypted: %d %q", res.StatusCode, body)
	}
	res, _ = rawGet(t, f.srv.URL+"/api/v1/admin/taxdocs/file?id=999", f.adminKey)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing file: %d want 404", res.StatusCode)
	}
}

// The FR-17 gate through the portal flow: a self-upload never clears the
// block. Whatever the row was (pending, received, validated, rejected,
// not_required), an upload lands 'pending' and blocks batching until the
// operator reviews it; only the operator's received / validated /
// not_required unblocks. An admin rejection blocks a staged payout's
// approval too.
func TestTaxDocUpload_PayoutGate(t *testing.T) {
	f := setupAPI(t)
	run := f.srv.URL + "/api/v1/admin/payouts/run"
	aff1Batched := func() bool {
		t.Helper()
		code, out := paPost(t, run, `{"min_threshold_cents":0,"dry_run":true}`, f.adminKey)
		if code != 200 {
			t.Fatalf("dry run: %d %v", code, out)
		}
		for _, b := range out["batches"].([]any) {
			if b.(map[string]any)["affiliate_id"].(float64) == 1 {
				return true
			}
		}
		return false
	}
	upload := func(name string) {
		t.Helper()
		code, out := uploadTaxDoc(t, f.srv.URL+"/api/v1/affiliate/taxdocs", f.aff1Key, "w9", name, testPDF)
		if code != 201 || out["status"] != "pending" {
			t.Fatalf("upload %s: %d %v, want 201 status=pending", name, code, out)
		}
	}
	if !aff1Batched() {
		t.Fatal("baseline: affiliate 1 (approved $30) should batch")
	}
	// First upload, no row yet: pending, blocked.
	upload("w9.pdf")
	if aff1Batched() {
		t.Fatal("unreviewed upload must block")
	}
	adminSetTaxDoc(t, f, 1, "w9", "received")
	if !aff1Batched() {
		t.Fatal("operator 'received' must unblock")
	}
	// Uploading over every operator status puts it back to pending.
	for _, prior := range []string{"received", "validated", "rejected", "not_required", "pending"} {
		adminSetTaxDoc(t, f, 1, "w9", prior)
		upload("w9-over-" + prior + ".pdf")
		if aff1Batched() {
			t.Fatalf("upload over %s must block until reviewed", prior)
		}
	}
	adminSetTaxDoc(t, f, 1, "w9", "rejected")
	if aff1Batched() {
		t.Fatal("rejected W-9 must block")
	}

	// Stage a real batch while validated, then reject: approval refuses
	// and the payout stays 'requested'.
	adminSetTaxDoc(t, f, 1, "w9", "validated")
	if code, out := paPost(t, run, `{"min_threshold_cents":0}`, f.adminKey); code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	var pid int64
	if err := f.db.DB.QueryRow(`SELECT id FROM payouts WHERE affiliate_id = 1 AND status = 'requested'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	adminSetTaxDoc(t, f, 1, "w9", "rejected")
	code, _ := paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", fmt.Sprintf(`{"payout_id":%d}`, pid), f.adminKey)
	var st string
	f.db.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pid).Scan(&st)
	if code != http.StatusConflict || st != "requested" {
		t.Fatalf("approve under rejected W-9: %d status=%s want 409/requested", code, st)
	}
	adminSetTaxDoc(t, f, 1, "w9", "validated")
	if code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", fmt.Sprintf(`{"payout_id":%d}`, pid), f.adminKey); code != 200 {
		t.Fatalf("approve after validation: %d %v", code, out)
	}
}
