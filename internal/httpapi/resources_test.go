package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// The owner adds text, a link and an image; partners list them; the image
// is public; SVG and bad input are refused; remove works.
func TestResources(t *testing.T) {
	f := setupAPI(t)
	png := tinyPNG(t)
	add := func(body string) int {
		t.Helper()
		code, _ := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/resources", f.adminKey, body)
		return code
	}
	for _, bad := range []string{`{"kind":"text","title":"","body":"x"}`, `{"kind":"text","title":"T","body":""}`, `{"kind":"link","title":"L","body":"javascript:alert(1)"}`, `{"kind":"image","title":"I","body":"x"}`} {
		if code := add(bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if add(`{"kind":"text","title":"Intro","body":"Try it: {link}"}`) != 201 || add(`{"kind":"link","title":"Pricing","body":"https://acme.test/pricing"}`) != 201 {
		t.Fatal("add text/link")
	}
	upload := func(title string, data []byte) int {
		t.Helper()
		req, _ := http.NewRequest("POST", f.srv.URL+"/api/v1/admin/resources/image?title="+title, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+f.adminKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := upload("Evil", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)); code != 415 {
		t.Fatalf("svg: %d", code)
	}
	if code := upload("Banner", png); code != 201 {
		t.Fatalf("png: %d", code)
	}
	if code, _ := sendJSON(t, "POST", f.srv.URL+"/api/v1/admin/resources", f.aff1Key, `{"kind":"text","title":"x","body":"y"}`); code == 201 {
		t.Fatal("a partner added a resource")
	}
	_, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/resources", f.aff1Key)
	list := m["resources"].([]any)
	if len(list) != 3 {
		t.Fatalf("partner list: %v", list)
	}
	img := list[2].(map[string]any)
	if img["kind"] != "image" || img["image_url"] == nil {
		t.Fatalf("image: %v", img)
	}
	resp, err := http.Get(f.srv.URL + img["image_url"].(string)) // no auth: public
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, png) {
		t.Fatalf("image get: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if code, _ := sendJSON(t, "DELETE", f.srv.URL+"/api/v1/admin/resources?id="+strconv.FormatInt(int64(img["id"].(float64)), 10), f.adminKey, ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if resp, _ := http.Get(f.srv.URL + img["image_url"].(string)); resp.StatusCode != 404 {
		t.Fatalf("deleted image still served: %d", resp.StatusCode)
	}
}
