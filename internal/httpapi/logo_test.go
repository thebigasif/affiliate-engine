package httpapi

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

func sendLogo(t *testing.T, method, url, key string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "text/html") // ignored: the type is sniffed
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// An uploaded logo is served same-origin and becomes the portal logo;
// removing it falls back to the logo_url (or none).
func TestBrandLogo_UploadServeRemove(t *testing.T) {
	f := setupAPI(t)
	seedCampaign(t, f, 1, "one")
	url := f.srv.URL + "/api/v1/admin/branding/logo?campaign_id=1"
	pngBytes := tinyPNG(t)

	code, m := sendLogo(t, "PUT", url, f.adminKey, pngBytes)
	if code != http.StatusOK {
		t.Fatalf("upload code=%d body=%v", code, m)
	}
	path, _ := m["logo_url"].(string)
	if !strings.HasPrefix(path, "/api/v1/portal/logo?campaign_id=1&v=") {
		t.Fatalf("logo_url = %q", path)
	}

	// The portal brand read points at it.
	_, b := getJSON(t, f.srv.URL+"/api/v1/portal/brand", "")
	if b["logo_url"] != path {
		t.Fatalf("brand logo_url = %v, want %s", b["logo_url"], path)
	}
	// The admin read shows it separately, so a PUT of the record never
	// stores the relative path as logo_url.
	_, a := getJSON(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey)
	if a["uploaded_logo"] != path || a["logo_url"] != "" {
		t.Fatalf("admin branding = %v", a)
	}
	if code, m := putBranding(t, f.srv.URL+"/api/v1/admin/branding?campaign_id=1", f.adminKey, a); code != http.StatusOK {
		t.Fatalf("round-trip PUT code=%d body=%v", code, m)
	}

	resp, err := http.Get(f.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, pngBytes) {
		t.Fatalf("serve code=%d len=%d", resp.StatusCode, len(got))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("versioned logo cache-control %q", cc)
	}

	if code, _ := sendLogo(t, "DELETE", url, f.adminKey, nil); code != http.StatusOK {
		t.Fatalf("delete code=%d", code)
	}
	if _, b := getJSON(t, f.srv.URL+"/api/v1/portal/brand", ""); b["logo_url"] != "" {
		t.Fatalf("after delete logo_url = %v", b["logo_url"])
	}
	if resp, _ := http.Get(f.srv.URL + path); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete serve code=%d", resp.StatusCode)
	}
}

// Only real raster images are stored: SVG/HTML are refused whatever the
// request claims, and so are oversized files and unknown programs.
func TestBrandLogo_Validation(t *testing.T) {
	f := setupAPI(t)
	seedCampaign(t, f, 1, "one")
	url := f.srv.URL + "/api/v1/admin/branding/logo?campaign_id=1"
	cases := []struct {
		name string
		url  string
		body []byte
		want int
	}{
		{"svg", url, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), http.StatusUnsupportedMediaType},
		{"html", url, []byte(`<html><script>alert(1)</script></html>`), http.StatusUnsupportedMediaType},
		{"empty", url, nil, http.StatusBadRequest},
		{"too big", url, append(tinyPNG(t), make([]byte, maxLogoBytes)...), http.StatusRequestEntityTooLarge},
		{"no program", f.srv.URL + "/api/v1/admin/branding/logo?campaign_id=9", tinyPNG(t), http.StatusNotFound},
	}
	for _, c := range cases {
		if code, m := sendLogo(t, "PUT", c.url, f.adminKey, c.body); code != c.want {
			t.Errorf("%s: code=%d want %d body=%v", c.name, code, c.want, m)
		}
	}
	// Partners can't upload.
	if code, _ := sendLogo(t, "PUT", url, "", tinyPNG(t)); code != http.StatusUnauthorized {
		t.Errorf("no key: code=%d", code)
	}
}
