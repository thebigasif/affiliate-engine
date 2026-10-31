// First-party tracking script served at /t.js (see t.js for what it does
// on the merchant's page).
//
// Design constraints:
//   - Static: the engine origin is read client-side from the script's own
//     src, so no request data (Host included) is reflected into the body
//     and one cached copy fits every host.
//   - No cookies from this route: the script sets its cookie on the
//     merchant's site, first-party.
//   - Writes nothing: clicks are still recorded only by /r/, with its rate
//     limit and allowlist.
package httpapi

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"time"
)

//go:embed t.js
var trackerJS []byte

// trackerETag lets browsers revalidate after max-age without a re-download.
var trackerETag = func() string {
	sum := sha256.Sum256(trackerJS)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// NewTrackerHandler serves the first-party tracking script.
func NewTrackerHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/javascript; charset=utf-8")
		// Short: a fixed script still has to reach every site soon after
		// an upgrade.
		h.Set("Cache-Control", "public, max-age=300")
		h.Set("X-Content-Type-Options", "nosniff")
		// Loaded cross-site by design: sites that require CORP (COEP) must
		// still be able to use it.
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("ETag", trackerETag)
		http.ServeContent(w, r, "t.js", time.Time{}, bytes.NewReader(trackerJS))
	}
}
