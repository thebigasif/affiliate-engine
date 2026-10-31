package httpapi_test

// Portal form POSTs carry the partner's session cookie, and SameSite=Lax
// still sends it from a sibling workspace (same site). They must come
// from the portal's own origin.

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
)

func portalPost(t *testing.T, u, contentType string, body io.Reader, cookie *http.Cookie, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", u, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPortal_CrossOriginFormPostsRefused(t *testing.T) {
	newsKey := []byte("0123456789abcdef0123456789abcdef")
	srv, h, adminKey := portalEnvWith(t, func(d *httpapi.APIDeps) {
		d.TaxDocKey = make([]byte, 32)
		d.NewsKey = newsKey
	})
	grant := approveNewAffiliate(t, srv.URL, adminKey, "csrf@partner.dev")
	_, out := postJSON(t, srv.URL+"/api/v1/portal/login", `{"grant":"`+grant+`"}`, "")
	session, _ := out["session"].(string)
	affID := int64(out["affiliate_id"].(float64))
	cookie := &http.Cookie{Name: "ae_portal_session", Value: session}

	upload := func(hdr map[string]string) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("doc_type", "w9")
		fw, _ := mw.CreateFormFile("file", "w9.pdf")
		_, _ = fw.Write([]byte("%PDF-1.4\n%%EOF\n"))
		_ = mw.Close()
		return portalPost(t, srv.URL+"/api/v1/affiliate/taxdocs", mw.FormDataContentType(), &buf, cookie, hdr)
	}
	sibling := map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.at.test"}
	oldBrowser := map[string]string{"Origin": "https://evil.example"}
	own := map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": srv.URL}
	for _, hdr := range []map[string]string{sibling, oldBrowser, {"Sec-Fetch-Site": "cross-site"}} {
		if code := upload(hdr); code != http.StatusForbidden {
			t.Fatalf("tax form upload with %v: %d want 403", hdr, code)
		}
	}
	if code := upload(own); code != http.StatusCreated {
		t.Fatalf("own-origin upload: %d", code)
	}

	unsub := func(hdr map[string]string) int {
		form := url.Values{"t": {partnermail.UnsubscribeToken(newsKey, affID)}}.Encode()
		return portalPost(t, srv.URL+"/api/v1/portal/unsubscribe", "application/x-www-form-urlencoded", strings.NewReader(form), nil, hdr)
	}
	optedOut := func() bool {
		var n int
		_ = h.DB.QueryRow(`SELECT news_opt_out FROM affiliates WHERE id = ?`, affID).Scan(&n)
		return n == 1
	}
	if code := unsub(sibling); code != http.StatusForbidden || optedOut() {
		t.Fatalf("cross-origin unsubscribe: %d", code)
	}
	if code := unsub(nil); code != http.StatusOK || !optedOut() { // mail providers' one-click POST: no browser headers
		t.Fatalf("one-click unsubscribe: %d", code)
	}

	if code := portalPost(t, srv.URL+"/api/v1/portal/logout", "", nil, cookie, sibling); code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: %d", code)
	}
	if code, _ := getJSON(t, srv.URL+"/api/v1/affiliate/earnings", session); code != 200 {
		t.Fatalf("session after a refused logout: %d", code)
	}
}

// Over HTTPS the session cookie is __Host- (no sibling subdomain can plant
// or overwrite it); the old name no longer signs anyone in.
func TestPortal_HostPrefixedCookieWhenSecure(t *testing.T) {
	srv, _, adminKey := portalEnvWith(t, func(d *httpapi.APIDeps) { d.SecureCookies = true })
	grant := approveNewAffiliate(t, srv.URL, adminKey, "host@partner.dev")
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/portal/login", strings.NewReader(`{"grant":"`+grant+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var got *http.Cookie
	for _, c := range resp.Cookies() {
		got = c
	}
	if got == nil || got.Name != "__Host-ae_portal_session" || !got.Secure || got.Path != "/" || got.Domain != "" {
		t.Fatalf("session cookie: %+v", got)
	}
	earnings := func(name string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/affiliate/earnings", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: got.Value})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := earnings("__Host-ae_portal_session"); code != 200 {
		t.Fatalf("__Host- cookie: %d", code)
	}
	if code := earnings("ae_portal_session"); code != 401 {
		t.Fatalf("legacy cookie name over https: %d want 401", code)
	}
}
