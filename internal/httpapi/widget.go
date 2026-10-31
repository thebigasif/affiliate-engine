// Embedded widget (FR-33 "widget mode"): a small public JS file served at
// /widget.js that merchants paste into their site. It renders a branded
// "become an affiliate" launcher that links into the portal, carrying the
// campaign id so the portal skins itself correctly.
//
// Design constraints:
//   - No cookies, no tracking, no PII — it is a static launcher.
//   - Content is fixed-shape: the only variable is the origin (this
//     engine's own base URL) — no merchant-supplied strings are reflected
//     into the script body (data-* attributes are read client-side and
//     assigned via textContent/style, never innerHTML).
//   - Served WITHOUT X-Frame-Options — the whole point is embedding.
package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// WidgetConfig shapes the served script.
type WidgetConfig struct {
	// BaseURL is this engine's external origin (e.g. "https://af.mysaas.com").
	// Defaults to the request's own origin when empty.
	BaseURL string
}

const widgetJS = `(function () {
  "use strict";
  if (window.__aeWidget) return; // idempotent embed
  var script = document.currentScript;
  var origin = %[1]q;
  if (!origin && script) origin = new URL(script.src).origin;
  var campaign = (script && script.getAttribute("data-campaign")) || "1";
  var label = (script && script.getAttribute("data-label")) || "Become an affiliate";
  function mount() {
    if (document.getElementById("ae-widget-launcher")) return;
    var a = document.createElement("a");
    a.id = "ae-widget-launcher";
    a.href = origin + "/?campaign=" + encodeURIComponent(campaign) + "#portal";
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    a.textContent = label;
    a.style.cssText = [
      "position:fixed", "right:24px", "bottom:24px", "z-index:2147483000",
      "background:#2563eb", "color:#fff", "padding:12px 20px",
      "border-radius:999px", "font:600 14px system-ui,sans-serif",
      "text-decoration:none", "box-shadow:0 4px 14px rgba(0,0,0,.25)"
    ].join(";");
    document.body.appendChild(a);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", mount);
  } else {
    mount();
  }
  window.__aeWidget = true;
})();
`

// NewWidgetHandler serves the embeddable launcher script.
func NewWidgetHandler(cfg WidgetConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := cfg.BaseURL
		fromHost := false
		if origin == "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			origin = fmt.Sprintf("%s://%s", scheme, r.Host)
			fromHost = true
		}
		origin = strings.TrimRight(origin, "/")
		// Defense-in-depth: the origin is embedded into the script (Go %q
		// quoting blocks string escape) and lands in an anchor href
		// client-side. A misconfigured AE_PUBLIC_URL ("javascript:...")
		// must never reach that href — require an absolute http(s) URL.
		if u, err := url.Parse(origin); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			origin = fmt.Sprintf("%s://%s", scheme, r.Host)
			fromHost = true
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		if fromHost {
			// The body embeds the request's Host: a shared cache must never
			// serve one Host's script for another (cache poisoning).
			w.Header().Set("Vary", "Host")
			w.Header().Set("Cache-Control", "private")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, widgetJS, origin) // #nosec G705 -- origin is %q-quoted into a JS string AND scheme-validated to http(s) against a parsed URL above; XSS corpus + B6 pin it
	}
}
