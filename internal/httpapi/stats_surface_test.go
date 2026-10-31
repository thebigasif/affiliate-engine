package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
)

// Built-but-invisible pin: the token drill + uniques endpoints surface the
// analytics that had zero API exposure (PRD audit finding).
func TestStatsSurfaces_DrillAndUniques(t *testing.T) {
	h := dbtest.Open(t)
	ts := analytics.NewTokenStats(h.DB)

	// Seed: clicks with utm dims + a uniq_daily row, then roll up.
	day := fmt.Sprintf("%d%02d%02d", time.Now().UTC().Year(), int(time.Now().UTC().Month()), time.Now().UTC().Day())
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (9, 's@t.dev')`)
	for _, src := range []string{"newsletter", "twitter"} {
		h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, utm_source)
			VALUES (?, ?, ?, ?, 9, 1, ?)`,
			fmt.Sprintf("sc-%s-%d", day, len(src)), time.Now().UTC().Format(time.RFC3339Nano), day, day[:6], src)
	}
	h.DB.Exec(`INSERT INTO uniq_daily (day, affiliate_id, ip_hash) VALUES (?, 9, x'0102')`, day)
	if _, err := ts.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{DB: h.DB, Stats: ts}))
	defer srv.Close()

	// Drill through HTTP.
	res, err := http.Get(srv.URL + "/api/v1/affiliate/stats/drill?dim=utm_source")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusServiceUnavailable {
		t.Fatal("stats not wired")
	}
	// Without auth this must 401 (the surface exists and is protected).
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("drill unauthenticated: %d want 401", res.StatusCode)
	}

	res2, err := http.Get(srv.URL + "/api/v1/affiliate/uniques")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("uniques unauthenticated: %d want 401", res2.StatusCode)
	}
}
