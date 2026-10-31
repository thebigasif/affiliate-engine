package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/gamify"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// One seed serves the read-only tests (a seed replays 90 days through the
// real engines; doing it per test would dominate the suite).
var demoShared struct {
	once sync.Once
	dir  string
	path string
	res  demoSeedResult
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if demoShared.dir != "" {
		_ = os.RemoveAll(demoShared.dir)
	}
	os.Exit(code)
}

func sharedDemo(t *testing.T) (string, demoSeedResult) {
	t.Helper()
	demoShared.once.Do(func() {
		demoShared.dir, demoShared.err = os.MkdirTemp("", "ae-demo-*")
		if demoShared.err != nil {
			return
		}
		demoShared.path = filepath.Join(demoShared.dir, "demo.db")
		demoShared.res, demoShared.err = seedDemo(context.Background(), demoShared.path, time.Now().UTC())
	})
	if demoShared.err != nil {
		t.Fatalf("seed: %v", demoShared.err)
	}
	return demoShared.path, demoShared.res
}

func openDemo(t *testing.T, path string) *db.Handles {
	t.Helper()
	h, err := db.Open(context.Background(), "sqlite:"+path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeHandles(h) })
	return h
}

// demoStoryCodes are the accounts the fraud stories use; every verdict and
// hold must land on one of them (no false positives on ordinary affiliates).
func demoStoryCodes() map[string]bool {
	out := map[string]bool{}
	for _, a := range demoRoster() {
		if a.story {
			out[a.code] = true
		}
	}
	return out
}

func TestDemoSeed_FraudStoryPerRule(t *testing.T) {
	path, _ := sharedDemo(t)
	h := openDemo(t, path)

	// Every rule the engine registers must have a story, except the ones
	// no real code path can feed (see planStories).
	unseedable := map[string]bool{"ip_fallback_anomaly": true}
	var want []string
	for _, r := range fraud.NewEngine(h.DB).Rules() {
		if !unseedable[r.Name()] {
			want = append(want, r.Name())
		}
	}
	if len(want) != 12 {
		t.Fatalf("ruleset has %d seedable rules, stories cover 12: add a story for the new rule", len(want))
	}
	for _, rule := range want {
		var verdicts, holds int
		var reason string
		if err := h.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(reason), '') FROM fraud_verdicts WHERE rule = ? AND dry_run = FALSE`, rule).
			Scan(&verdicts, &reason); err != nil {
			t.Fatal(err)
		}
		// Hold reasons list the rules: "fraud review: a,b".
		if err := h.DB.QueryRow(`
			SELECT COUNT(*) FROM commission_holds
			WHERE resolved_at IS NULL AND ',' || substr(reason, 15) || ',' LIKE '%,' || ? || ',%'`, rule).
			Scan(&holds); err != nil {
			t.Fatal(err)
		}
		if verdicts == 0 || holds == 0 || reason == "" {
			t.Errorf("%s: %d verdicts, %d open holds, reason %q", rule, verdicts, holds, reason)
		}
	}
	for rule := range unseedable {
		var n int
		_ = h.DB.QueryRow(`SELECT COUNT(*) FROM fraud_verdicts WHERE rule = ?`, rule).Scan(&n)
		if n != 0 {
			t.Errorf("%s fired %d times: no story should reach it", rule, n)
		}
	}

	// Only story accounts are held, and every affiliate-subject verdict
	// names one of them.
	story := demoStoryCodes()
	rows, err := h.DB.Query(`
		SELECT ac.code FROM commission_holds ch
		JOIN commissions c ON c.id = ch.commission_id
		JOIN affiliate_codes ac ON ac.affiliate_id = c.affiliate_id
		UNION ALL
		SELECT ac.code FROM fraud_verdicts v
		JOIN affiliate_codes ac ON ac.affiliate_id = CAST(replace(v.subject, 'affiliate:', '') AS INTEGER)
		WHERE v.subject NOT LIKE 'ip:%' AND v.subject GLOB '*[0-9]'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		seen++
		if !story[code] {
			t.Errorf("fraud verdict or hold on ordinary affiliate %q", code)
		}
	}
	if seen == 0 {
		t.Fatal("no holds or verdicts joined back to affiliates")
	}
}

func TestDemoSeed_DoctorPasses(t *testing.T) {
	path, _ := sharedDemo(t)
	h := openDemo(t, path)
	var out strings.Builder
	ok, err := doctor(context.Background(), h, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || strings.Contains(out.String(), "FAIL") {
		t.Fatalf("doctor on the seeded database:\n%s", out.String())
	}
}

// The program reads like a real one: statuses, refunds on every parent
// state, both payout runs, recurring revenue, traffic variety, recent dates.
func TestDemoSeed_Program(t *testing.T) {
	path, res := sharedDemo(t)
	h := openDemo(t, path)
	c := res.counts
	for name, want := range map[string]int64{"affiliates": 40, "affiliates_frozen": 1, "applications_pending": 4, "payout_runs": 2} {
		if c[name] != want {
			t.Errorf("%s = %d, want %d", name, c[name], want)
		}
	}
	for _, name := range []string{"clicks", "commissions_pending", "commissions_approved", "commissions_paid", "payouts_paid", "payouts_requested", "badges", "xp"} {
		if c[name] == 0 {
			t.Errorf("%s = 0", name)
		}
	}
	one := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := h.DB.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	checks := []struct {
		what string
		n    int64
		min  int64
	}{
		{"frozen account is the coupon hijacker", one(`SELECT COUNT(*) FROM affiliates WHERE status = 'frozen' AND email = 'deals@dealdrop.example'`), 1},
		{"refund while pending (parent cancelled, no maturation)", one(`
			SELECT COUNT(*) FROM commissions k JOIN commissions p ON p.id = k.parent_commission_id
			WHERE p.status = 'cancelled' AND NOT EXISTS (SELECT 1 FROM journal_entries WHERE idem_key = 'mature:' || p.id)`), 1},
		{"refund after maturation (clawed from the payable)", one(`
			SELECT COUNT(*) FROM commissions k JOIN commissions p ON p.id = k.parent_commission_id
			WHERE p.status = 'cancelled' AND EXISTS (SELECT 1 FROM journal_entries WHERE idem_key = 'mature:' || p.id)`), 1},
		{"partial refund", one(`
			SELECT COUNT(*) FROM commissions k JOIN commissions p ON p.id = k.parent_commission_id
			WHERE p.status IN ('approved','paid') AND k.amount < 0`), 1},
		{"refund after payout, netted into the pending run", one(`
			SELECT COUNT(*) FROM commissions k JOIN commissions p ON p.id = k.parent_commission_id
			JOIN payout_lines pl ON pl.commission_id = k.id JOIN payouts po ON po.id = pl.payout_id
			WHERE p.status = 'clawback' AND po.status = 'requested'`), 1},
		{"pending W-9 keeps its affiliate out of payouts", one(`
			SELECT COUNT(*) FROM tax_documents td WHERE td.status = 'pending'
			AND NOT EXISTS (SELECT 1 FROM payouts WHERE affiliate_id = td.affiliate_id)
			AND EXISTS (SELECT 1 FROM commissions WHERE affiliate_id = td.affiliate_id AND status = 'approved')`), 1},
		{"monthly renewals", one(`SELECT COUNT(*) FROM raw_events WHERE event_type = 'renewal'`), 100},
		{"utm_source values", one(`SELECT COUNT(DISTINCT utm_source) FROM clicks`), 8},
		{"subid values", one(`SELECT COUNT(DISTINCT subid) FROM clicks WHERE subid != ''`), 10},
		{"countries", one(`SELECT COUNT(DISTINCT country) FROM clicks`), 10},
		{"known bots stored but excluded", one(`SELECT COUNT(*) FROM clicks WHERE exclude_stats = 1`), 1},
		{"token-dimension rollup", one(`SELECT COUNT(*) FROM token_stats_daily`), 1},
		{"Teams bounties (campaign 2)", one(`SELECT COUNT(*) FROM commissions WHERE campaign_id = 2 AND amount = 7500`), 1},
		{"volume-tier rate reached", one(`SELECT COUNT(*) FROM commissions WHERE rate_snapshot LIKE '%volume_tier%'`), 1},
	}
	for _, ch := range checks {
		if ch.n < ch.min {
			t.Errorf("%s: %d, want >= %d", ch.what, ch.n, ch.min)
		}
	}

	// History covers the last 90 days and stops before today.
	end := time.Now().UTC().Truncate(24 * time.Hour)
	var first, last string
	if err := h.DB.QueryRow(`SELECT MIN(ts), MAX(ts) FROM clicks`).Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	f, _ := time.Parse(time.RFC3339Nano, first)
	l, _ := time.Parse(time.RFC3339Nano, last)
	if f.Before(end.AddDate(0, 0, -demoDays)) || !l.Before(end) || l.Before(end.Add(-24*time.Hour)) {
		t.Fatalf("clicks span %s..%s, want the %d days before %s", first, last, demoDays, end)
	}
}

// The dashboard over the seeded database with the demo key, wired like
// main: every admin read works, the fraud queue shows each story, and the
// ledger proof holds for every affiliate the portal can show.
func TestDemoSeed_ServedReadOnly(t *testing.T) {
	path, _ := sharedDemo(t)
	h := openDemo(t, path)
	api := httptest.NewServer(demoGuard(httpapi.NewAPIHandler(httpapi.APIDeps{
		DB:     h.DB,
		Gamify: gamify.New(h.DB),
		Stats:  analytics.NewTokenStats(h.DB),
		Demo:   true,
	}), true))
	t.Cleanup(api.Close)

	get := func(p string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", api.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+httpapi.DemoKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %v", p, resp.StatusCode, err)
		}
		return m
	}

	q := get("/api/v1/admin/fraud/queue")
	rules := map[string]bool{}
	for _, v := range q["verdicts"].([]any) {
		rules[v.(map[string]any)["rule"].(string)] = true
	}
	if len(rules) != 12 || len(q["holds"].([]any)) == 0 {
		t.Fatalf("fraud queue: rules %v, %d holds", slices.Sorted(maps.Keys(rules)), len(q["holds"].([]any)))
	}
	if d := get("/api/v1/admin/dashboard"); d["ledger_integrity_sum"] != float64(0) {
		t.Fatalf("dashboard integrity: %v", d["ledger_integrity_sum"])
	}

	brand := get("/api/v1/portal/brand")
	demo := brand["demo"].(map[string]any)
	if brand["brand_name"] != "Northwind Notes Partners" || demo["affiliate_id"] == nil {
		t.Fatalf("brand: %v", brand)
	}
	aff := int64(demo["affiliate_id"].(float64))
	get(fmt.Sprintf("/api/v1/affiliate/profile?affiliate_id=%d", aff))
	get(fmt.Sprintf("/api/v1/affiliate/stats/drill?affiliate_id=%d", aff))
	get("/api/v1/leaderboard")

	affs := get("/api/v1/admin/affiliates")["affiliates"].([]any)
	if len(affs) != 40 {
		t.Fatalf("affiliates list: %d", len(affs))
	}
	for _, a := range affs {
		id := int64(a.(map[string]any)["id"].(float64))
		e := get(fmt.Sprintf("/api/v1/affiliate/earnings?affiliate_id=%d", id))
		if proof := e["ledger_proof"].(map[string]any); proof["holds"] != true {
			t.Fatalf("affiliate %d ledger proof: %v (payable %v)", id, proof, e["payable_balance_cents"])
		}
	}

	// Writes and the webhook/MCP/click surfaces stay shut.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/admin/fraud/resolve"}, {"POST", "/api/v1/admin/payouts/run"}, {"POST", "/webhooks/stripe"}, {"POST", "/mcp"},
	} {
		req, _ := http.NewRequest(c.method, api.URL+c.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+httpapi.DemoKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s: %d want 403", c.method, c.path, resp.StatusCode)
		}
	}
}

// -reset wipes a seeded demo database and seeds it again; the program is
// the same every time (counts match), only dates move.
func TestDemoSeed_ResetIsDeterministic(t *testing.T) {
	if raceEnabled {
		// A seed replays 90 days through pure-Go SQLite, ~25x slower under
		// -race; the shared seed already runs the same code under it.
		t.Skip("second full seed skipped under -race")
	}
	path, res := sharedDemo(t)
	dst := filepath.Join(t.TempDir(), "again.db")
	if err := copyFile(path, dst); err != nil {
		t.Fatal(err)
	}
	if err := runDemoSeed("sqlite:"+dst, []string{"-reset"}); err != nil {
		t.Fatal(err)
	}
	h := openDemo(t, dst)
	again, err := countDemo(context.Background(), h.DB)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(res.counts, again) {
		for _, k := range slices.Sorted(maps.Keys(res.counts)) {
			if res.counts[k] != again[k] {
				t.Errorf("%s: first seed %d, reseed %d", k, res.counts[k], again[k])
			}
		}
		t.Fatalf("reseed differs")
	}
}

func migratedDB(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.db")
	h := openDemo(t, path)
	if err := migrations.Apply(context.Background(), h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := h.DB.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

const demoMarkerDDL = `CREATE TABLE demo_seed (id INTEGER PRIMARY KEY, anchor TEXT NOT NULL, started_at TEXT NOT NULL, seeded_at TEXT NOT NULL DEFAULT '')`

func affiliateCount(t *testing.T, path string) int {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM affiliates`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDemoSeed_RefusesNonEmptyDB(t *testing.T) {
	real := migratedDB(t, `INSERT INTO affiliates (email) VALUES ('owner@shop.example')`)
	err := runDemoSeed("sqlite:"+real, nil)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("seed into a non-empty db: %v", err)
	}
	// -reset only wipes what demo-seed created.
	err = runDemoSeed("sqlite:"+real, []string{"-reset"})
	if err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("-reset on a real db: %v", err)
	}
	if n := affiliateCount(t, real); n != 1 {
		t.Fatalf("refused seed changed the db: %d affiliates", n)
	}
	// A demo database still needs -reset to be seeded again.
	demo := migratedDB(t, demoMarkerDDL,
		`INSERT INTO demo_seed (id, anchor, started_at) VALUES (1, 'x', 'x')`,
		`INSERT INTO affiliates (email) VALUES ('a@example.com')`)
	if err := runDemoSeed("sqlite:"+demo, nil); err == nil || !strings.Contains(err.Error(), "-reset") {
		t.Fatalf("seed into a demo db without -reset: %v", err)
	}
	// Never the default database, never postgres.
	for _, dsn := range []string{"", "sqlite:"} {
		if err := runDemoSeed(dsn, nil); err == nil || !strings.Contains(err.Error(), "-db") {
			t.Fatalf("dsn %q: %v", dsn, err)
		}
	}
	if serve, code := dispatchCLI([]string{"demo-seed"}, "postgres://u:pw@127.0.0.1:1/ae"); serve || code != 1 {
		t.Fatalf("demo-seed on postgres: serve=%v code=%d", serve, code)
	}
}

func TestDemoNeedsSeed(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		st     demoDBState
		reseed bool
		err    bool
	}{
		{"missing file", demoDBState{empty: true}, true, false},
		{"empty db", demoDBState{exists: true, empty: true}, true, false},
		{"someone else's data", demoDBState{exists: true}, false, true},
		{"half-finished seed", demoDBState{exists: true, marked: true}, true, false},
		{"fresh seed", demoDBState{exists: true, marked: true, complete: true, seededAt: now.Add(-2 * time.Hour)}, false, false},
		{"day-old seed", demoDBState{exists: true, marked: true, complete: true, seededAt: now.Add(-23 * time.Hour)}, true, false},
	} {
		reseed, err := demoNeedsSeed(c.st, now)
		if reseed != c.reseed || (err != nil) != c.err {
			t.Errorf("%s: reseed=%v err=%v", c.name, reseed, err)
		}
	}
}

// Serving: a fresh demo seed is kept (no wipe); a database demo-seed did
// not create is never touched.
func TestEnsureDemoSeed_KeepsFreshRefusesForeign(t *testing.T) {
	ctx := context.Background()
	seeded := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	demo := migratedDB(t, demoMarkerDDL,
		fmt.Sprintf(`INSERT INTO demo_seed (id, anchor, started_at, seeded_at) VALUES (1, 'x', 'x', '%s')`, seeded.Format(time.RFC3339)),
		`INSERT INTO affiliates (email) VALUES ('a@example.com')`)
	got, err := ensureDemoSeed(ctx, demo, time.Now().UTC())
	if err != nil || !got.Equal(seeded) || affiliateCount(t, demo) != 1 {
		t.Fatalf("fresh seed: at=%v err=%v", got, err)
	}
	foreign := migratedDB(t, `INSERT INTO affiliates (email) VALUES ('owner@shop.example')`)
	if _, err := ensureDemoSeed(ctx, foreign, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("foreign db: %v", err)
	}
	if affiliateCount(t, foreign) != 1 {
		t.Fatal("foreign db was modified")
	}
}

func TestDemoRestartAt(t *testing.T) {
	for _, c := range []struct{ seeded, want string }{
		{"2026-09-10T04:00:30Z", "2026-09-11T04:00:00Z"}, // steady state: daily at 04:00
		{"2026-09-10T03:00:00Z", "2026-09-11T04:00:00Z"},
		{"2026-09-10T16:00:00Z", "2026-09-12T04:00:00Z"}, // first 04:00 once a day old
		{"2026-09-10T05:00:00Z", "2026-09-11T04:00:00Z"},
	} {
		seeded, _ := time.Parse(time.RFC3339, c.seeded)
		if got := demoRestartAt(seeded).Format(time.RFC3339); got != c.want {
			t.Errorf("seeded %s: restart %s, want %s", c.seeded, got, c.want)
		}
	}
}

// AE_DEMO unset: no demo, the handler is the server's own mux.
func TestSetupDemo_OffUnlessSet(t *testing.T) {
	t.Setenv("AE_DEMO", "")
	on, err := setupDemo(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "x.db"), func() {})
	if on || err != nil {
		t.Fatalf("on=%v err=%v", on, err)
	}
	mux := http.NewServeMux()
	if demoGuard(mux, false) != http.Handler(mux) {
		t.Fatal("demoGuard wrapped the handler with demo mode off")
	}
	t.Setenv("AE_DEMO", "1")
	if _, err := setupDemo(context.Background(), "postgres://u:pw@127.0.0.1:1/ae", func() {}); err == nil {
		t.Fatal("demo mode accepted a postgres database")
	}
}
