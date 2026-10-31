// Read-only live demo (AE_DEMO=1). Anonymous prospects click around the
// real admin dashboard over a seeded database, behind two fences: every
// write method is refused before any handler runs, and the only principal
// that authenticates is the public demo key. A real API key (leaked, or
// copied from another install) gets nothing here, not even reads.
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

const (
	// DemoKey is the public bearer credential of a demo install. It is
	// meaningless outside demo mode: apikeys only knows hashed ae_ keys.
	DemoKey = "demo"
	// DemoTrialURL is where the demo banner sends prospects.
	DemoTrialURL = "https://affiliatetracking.co/#pricing"
	// DemoReadOnlyMessage answers every refused write.
	DemoReadOnlyMessage = "This is a read-only demo. Start a free trial to try it with your own program."
)

// DemoInfo is what the UI shell learns about a demo install, riding on
// the brand payload it already fetches at boot (no extra request, and a
// normal install's payload is unchanged).
type DemoInfo struct {
	Key string `json:"key"`
	// AffiliateID is whose portal the Portal tab shows (the demo key is
	// an admin, which reads affiliate views by ?affiliate_id=).
	AffiliateID int64  `json:"affiliate_id,omitempty"`
	TrialURL    string `json:"trial_url"`
}

// DemoGuard is the demo's front door. It wraps the whole server (main)
// and the API mux, so a route added later is covered without a demo check
// of its own:
//   - any method but GET/HEAD: 403 with DemoReadOnlyMessage;
//   - billing webhooks and MCP: 403 on every method (webhooks write
//     money; MCP hands out a tool surface);
//   - /r/: a tracked click is a write (click row, attribution, cookie)
//     and the demo has no merchant site behind its links, so a link
//     opens the dashboard instead.
func DemoGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method != http.MethodGet && r.Method != http.MethodHead,
			strings.HasPrefix(p, "/webhooks/"), p == "/mcp", strings.HasPrefix(p, "/mcp/"):
			apiError(w, http.StatusForbidden, DemoReadOnlyMessage)
		case strings.HasPrefix(p, "/r/"):
			http.Redirect(w, r, "/", http.StatusFound)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// demoAuth replaces key authentication in demo mode: DemoKey maps to a
// read-only admin (DemoGuard refuses its writes); every other key fails,
// counted against the same per-IP failure budget as normal auth. The key
// is shared by every visitor, so the request budget is per client IP.
func (a *api) demoAuth(w http.ResponseWriter, r *http.Request) *apikeys.Identity {
	ip := ClientIP(r, a.deps.TrustedProxies)
	failBucket := "authfail:" + ip
	if a.authFail.exceeded(failBucket) {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, "too many failed authentication attempts")
		return nil
	}
	if bearerToken(r) != DemoKey {
		a.authFail.allow(failBucket)
		apiError(w, http.StatusUnauthorized, "this live demo only accepts the public demo key")
		return nil
	}
	if !a.rl.allow("demo:" + ip) {
		apiError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return nil
	}
	return &apikeys.Identity{Scope: apikeys.ScopeAdmin, IsAdmin: true}
}

// demoInfo builds the brand payload's demo block. The Portal tab shows the
// top earner among active affiliates in the program currency (amounts in
// different currencies do not rank against each other; 0 = none yet, the
// tab then asks for an affiliate like it does for any admin key).
func (a *api) demoInfo(ctx context.Context) *DemoInfo {
	info := &DemoInfo{Key: DemoKey, TrialURL: DemoTrialURL}
	_ = a.db.QueryRowContext(ctx, `
		SELECT c.affiliate_id FROM commissions c
		JOIN affiliates af ON af.id = c.affiliate_id
		WHERE af.status = 'active' AND c.amount > 0 AND c.currency = ?
		GROUP BY c.affiliate_id ORDER BY SUM(c.amount) DESC, c.affiliate_id LIMIT 1`,
		settings.Currency(ctx, a.db)).Scan(&info.AffiliateID)
	return info
}
