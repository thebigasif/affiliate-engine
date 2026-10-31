package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Host-derived widget must not be shared-cacheable (its body embeds the
// request Host); a configured AE_PUBLIC_URL keeps it publicly cacheable.
func TestWidget_CacheHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	NewWidgetHandler(WidgetConfig{})(rec, httptest.NewRequest("GET", "/widget.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if v := rec.Header().Get("Vary"); v != "Host" {
		t.Fatalf("Vary=%q want Host", v)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private" {
		t.Fatalf("Cache-Control=%q want private", cc)
	}

	rec = httptest.NewRecorder()
	NewWidgetHandler(WidgetConfig{BaseURL: "https://af.acme.com"})(rec, httptest.NewRequest("GET", "/widget.js", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Fatalf("Cache-Control=%q want public", cc)
	}
	if v := rec.Header().Get("Vary"); v != "" {
		t.Fatalf("Vary=%q want empty for a fixed origin", v)
	}
}
