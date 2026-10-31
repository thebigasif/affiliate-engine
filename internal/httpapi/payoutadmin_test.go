package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func stageAff1Payout(t *testing.T, f *apiFixture) int64 {
	t.Helper()
	if code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/run", `{"min_threshold_cents":0,"method":"paypal"}`, f.adminKey); code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	var pid int64
	if err := f.db.DB.QueryRow(`SELECT id FROM payouts WHERE affiliate_id = 1 AND status = 'requested'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestPayoutList_And_CSVExport(t *testing.T) {
	f := setupAPI(t)
	pid := stageAff1Payout(t, f)

	code, out := getJSON(t, f.srv.URL+"/api/v1/admin/payouts", f.adminKey)
	list := out["payouts"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list requested: %d %v", code, out)
	}
	p := list[0].(map[string]any)
	if int64(p["id"].(float64)) != pid || p["amount_cents"].(float64) != 3000 || p["email"] != "aff1@x.test" || p["status"] != "requested" {
		t.Fatalf("payout row: %v", p)
	}
	_, out = getJSON(t, f.srv.URL+"/api/v1/admin/payouts?status=paid", f.adminKey)
	if n := len(out["payouts"].([]any)); n != 1 { // the fixture's settled payout
		t.Fatalf("paid list: %d", n)
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/payouts?status=bogus", f.adminKey); code != 400 {
		t.Fatalf("bad status: %d", code)
	}
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/payouts", f.aff1Key); code != http.StatusForbidden {
		t.Fatalf("affiliate list: %d want 403", code)
	}

	// The payout was staged with method=paypal: only the PayPal file
	// lists it, and only once.
	readCSV := func(rail string) (*http.Response, [][]string) {
		t.Helper()
		res, body := rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/"+rail, f.adminKey)
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") ||
			!strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
			t.Fatalf("%s: %d %v", rail, res.StatusCode, res.Header)
		}
		recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
		if err != nil {
			t.Fatalf("%s: %v %q", rail, err, body)
		}
		return res, recs
	}
	if _, recs := readCSV("wise.csv"); len(recs) != 1 || strings.Join(recs[0], ",") != "email,amount,currency,reference" {
		t.Fatalf("wise file listed a paypal payout: %v", recs)
	}
	res, recs := readCSV("paypal.csv")
	want := []string{"email", "amount", "currency", "note_id", "aff1@x.test", "30.00", "USD", fmt.Sprintf("payout-%d", pid)}
	if len(recs) != 2 || strings.Join(append(recs[0], recs[1]...), ",") != strings.Join(want, ",") {
		t.Fatalf("paypal: got %v want %v", recs, want)
	}
	if res.Header.Get("X-Payouts-Exported") != "1" || res.Header.Get("X-Export-Batch") == "" {
		t.Fatalf("paypal headers: %v", res.Header)
	}
	if _, recs := readCSV("paypal.csv"); len(recs) != 1 {
		t.Fatalf("re-export listed an exported payout again: %v", recs)
	}
	for _, rail := range []string{"paypal.csv", "wise.csv"} {
		if res, _ := rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/"+rail, f.aff1Key); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s affiliate: %d want 403", rail, res.StatusCode)
		}
	}
	if res, _ := rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/venmo.csv", f.adminKey); res.StatusCode != 404 {
		t.Fatalf("unknown rail: %d want 404", res.StatusCode)
	}
	// Exporting claimed the payout but moved no money: still 'requested',
	// marked exported, and the list shows it.
	var st string
	var exported bool
	f.db.DB.QueryRow(`SELECT status, exported_at IS NOT NULL FROM payouts WHERE id = ?`, pid).Scan(&st, &exported)
	if st != "requested" || !exported {
		t.Fatalf("after export: status=%s exported=%v", st, exported)
	}
	_, out = getJSON(t, f.srv.URL+"/api/v1/admin/payouts", f.adminKey)
	if p := out["payouts"].([]any)[0].(map[string]any); p["export_batch"] == "" || p["exported_at"] == "" {
		t.Fatalf("list lacks export claim: %v", p)
	}
	// Approve settles the exported payout.
	if code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", fmt.Sprintf(`{"payout_id":%d}`, pid), f.adminKey); code != 200 {
		t.Fatalf("approve exported: %d %v", code, out)
	}
}

// The export applies Approve's gate: a payout with an open fraud hold or a
// pending tax document is not in the file (and not claimed), so it can
// never be paid from a file while the ledger still says it is blocked.
func TestPayoutExport_AppliesApproveGate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		clear string
	}{
		{
			"hold",
			`INSERT INTO commission_holds (commission_id, reason) SELECT commission_id, 'flag' FROM payout_lines WHERE payout_id = ?`,
			`UPDATE commission_holds SET resolved_at = CURRENT_TIMESTAMP WHERE commission_id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)`,
		},
		{
			"taxdoc",
			`INSERT INTO tax_documents (affiliate_id, doc_type, status) SELECT affiliate_id, 'w9', 'pending' FROM payouts WHERE id = ?`,
			`UPDATE tax_documents SET status = 'validated' WHERE affiliate_id = (SELECT affiliate_id FROM payouts WHERE id = ?)`,
		},
		{
			"commission_left_approved",
			`UPDATE commissions SET status = 'cancelled' WHERE id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)`,
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupAPI(t)
			pid := stageAff1Payout(t, f)
			if _, err := f.db.DB.Exec(tc.block, pid); err != nil {
				t.Fatal(err)
			}
			res, body := rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/paypal.csv", f.adminKey)
			recs, _ := csv.NewReader(strings.NewReader(string(body))).ReadAll()
			if res.StatusCode != 200 || len(recs) != 1 || res.Header.Get("X-Payouts-Skipped") != "1" {
				t.Fatalf("blocked payout exported: %d %v %v", res.StatusCode, recs, res.Header)
			}
			var exported bool
			f.db.DB.QueryRow(`SELECT exported_at IS NOT NULL FROM payouts WHERE id = ?`, pid).Scan(&exported)
			if exported {
				t.Fatal("blocked payout was claimed")
			}
			if tc.clear == "" {
				return
			}
			if _, err := f.db.DB.Exec(tc.clear, pid); err != nil {
				t.Fatal(err)
			}
			_, body = rawGet(t, f.srv.URL+"/api/v1/admin/payouts/export/paypal.csv", f.adminKey)
			if recs, _ := csv.NewReader(strings.NewReader(string(body))).ReadAll(); len(recs) != 2 {
				t.Fatalf("cleared payout not exported: %v", recs)
			}
		})
	}
}

// Double click: concurrent Approve calls on one payout all answer 200,
// the payout is paid once, and exactly one settle journal posts.
func TestPayoutApprove_DoubleClickSafe(t *testing.T) {
	f := setupAPI(t)
	pid := stageAff1Payout(t, f)
	body := fmt.Sprintf(`{"payout_id":%d}`, pid)

	const clicks = 5
	codes := make([]int, clicks)
	var wg sync.WaitGroup
	for i := 0; i < clicks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = paPost(t, f.srv.URL+"/api/v1/admin/payouts/approve", body, f.adminKey)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("click %d: %d want 200", i, c)
		}
	}
	var st string
	var journals, paid int
	f.db.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pid).Scan(&st)
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE idem_key = ?`, fmt.Sprintf("payout:%d", pid)).Scan(&journals)
	f.db.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE affiliate_id = 1 AND status = 'paid'`).Scan(&paid)
	if st != "paid" || journals != 1 || paid != 2 {
		t.Fatalf("status=%s settle journals=%d paid commissions=%d; want paid/1/2", st, journals, paid)
	}
	var sum int64
	f.db.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&sum)
	if sum != 0 {
		t.Fatalf("ledger sum=%d", sum)
	}
}

func TestSettings_DefaultsAndMinPayout(t *testing.T) {
	f := setupAPI(t)
	url := f.srv.URL + "/api/v1/admin/settings"
	code, out := getJSON(t, url, f.adminKey)
	if code != 200 || out["min_payout_cents"].(float64) != 5000 || out["default_hold_days"].(float64) != 30 {
		t.Fatalf("defaults: %d %v", code, out)
	}
	if code, _ := getJSON(t, url, f.aff1Key); code != http.StatusForbidden {
		t.Fatalf("affiliate read settings: %d", code)
	}

	// Default $50 minimum: affiliate 1's $30 approved does not batch.
	dry := func(body string) (float64, int) {
		t.Helper()
		code, out := paPost(t, f.srv.URL+"/api/v1/admin/payouts/run", body, f.adminKey)
		if code != 200 {
			t.Fatalf("dry run: %d %v", code, out)
		}
		return out["min_threshold_cents"].(float64), len(out["batches"].([]any))
	}
	if th, n := dry(`{"dry_run":true}`); th != 5000 || n != 0 {
		t.Fatalf("default minimum: threshold=%v batches=%d; want 5000/0", th, n)
	}
	// Explicit per-run override still works.
	if th, n := dry(`{"dry_run":true,"min_threshold_cents":0}`); th != 0 || n != 1 {
		t.Fatalf("override: threshold=%v batches=%d", th, n)
	}
	// Lower the program minimum to $25: now it batches.
	put := func(body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("PUT", url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.adminKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var o map[string]any
		_ = jsonDecode(res, &o)
		return res.StatusCode, o
	}
	if code, out := put(`{"min_payout_cents":2500}`); code != 200 || out["default_hold_days"].(float64) != 30 {
		t.Fatalf("put: %d %v", code, out)
	}
	if th, n := dry(`{"dry_run":true}`); th != 2500 || n != 1 {
		t.Fatalf("after put: threshold=%v batches=%d", th, n)
	}
	// Program currency and per-currency minimums round-trip.
	if code, out := put(`{"currency":"eur","min_payout_by_currency":{"jpy":5000}}`); code != 200 ||
		out["currency"] != "EUR" || out["min_payout_by_currency"].(map[string]any)["JPY"].(float64) != 5000 {
		t.Fatalf("put currency: %d %v", code, out)
	}
	if code, out := getJSON(t, url, f.adminKey); code != 200 || out["currency"] != "EUR" ||
		out["min_payout_by_currency"].(map[string]any)["JPY"].(float64) != 5000 || out["min_payout_cents"].(float64) != 2500 {
		t.Fatalf("get after currency put: %d %v", code, out)
	}
	for _, bad := range []string{
		`{"min_payout_cents":-1}`, `{"default_hold_days":366}`, `{"default_hold_days":-1}`, `nope`,
		`{"currency":"dollars"}`, `{"min_payout_by_currency":{"JPY":-1}}`, `{"min_payout_by_currency":{"YEN!":1}}`,
	} {
		if code, _ := put(bad); code != 400 {
			t.Fatalf("put %s: %d want 400", bad, code)
		}
	}
	if code, _ := paPost(t, f.srv.URL+"/api/v1/admin/payouts/run", `{"min_threshold_cents":-5,"dry_run":true}`, f.adminKey); code != 400 {
		t.Fatalf("negative override: %d want 400", code)
	}
}

func TestSettings_HoldDaysForPrograms(t *testing.T) {
	f := setupAPI(t)
	// default_hold_days seeds new programs created without net_terms_days.
	req, _ := http.NewRequest("PUT", f.srv.URL+"/api/v1/admin/settings", strings.NewReader(`{"default_hold_days":14}`))
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("put: %v %v", err, res)
	}
	res.Body.Close()
	code, out := paPost(t, f.srv.URL+"/api/v1/admin/campaigns", `{"name":"p1"}`, f.adminKey)
	if code != 201 || out["net_terms_days"].(float64) != 14 {
		t.Fatalf("create default: %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	// Explicit 0 = no hold (was silently rewritten to 30).
	code, out = paPost(t, f.srv.URL+"/api/v1/admin/campaigns", `{"name":"p2","net_terms_days":0}`, f.adminKey)
	if code != 201 || out["net_terms_days"].(float64) != 0 {
		t.Fatalf("create zero: %d %v", code, out)
	}

	patch := func(path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("PATCH", f.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.adminKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var o map[string]any
		_ = jsonDecode(res, &o)
		return res.StatusCode, o
	}
	code, out = patch(fmt.Sprintf("/api/v1/admin/campaigns/%d", id), `{"net_terms_days":45}`)
	if code != 200 || out["net_terms_days"].(float64) != 45 || out["attribution_window_days"].(float64) != 30 {
		t.Fatalf("patch: %d %v", code, out)
	}
	_, out = getJSON(t, f.srv.URL+"/api/v1/admin/campaigns", f.adminKey)
	found := false
	for _, c := range out["campaigns"].([]any) {
		m := c.(map[string]any)
		if int64(m["id"].(float64)) == id && m["net_terms_days"].(float64) == 45 {
			found = true
		}
	}
	if !found {
		t.Fatalf("campaign list lacks net_terms_days=45: %v", out)
	}
	for path, body := range map[string]string{
		fmt.Sprintf("/api/v1/admin/campaigns/%d", id): `{"net_terms_days":400}`,
		"/api/v1/admin/campaigns/abc":                 `{"net_terms_days":10}`,
	} {
		if code, _ := patch(path, body); code != 400 {
			t.Fatalf("patch %s %s: %d want 400", path, body, code)
		}
	}
	if code, _ := patch(fmt.Sprintf("/api/v1/admin/campaigns/%d", id), `{}`); code != 400 {
		t.Fatalf("empty patch: %d", code)
	}
	if code, _ := patch("/api/v1/admin/campaigns/9999", `{"net_terms_days":10}`); code != 404 {
		t.Fatalf("unknown campaign: %d", code)
	}

	// Commission period: 0 (lifetime) by default, settable on create and
	// PATCH, bounded 0-120, and PATCH leaves the other fields alone.
	code, out = paPost(t, f.srv.URL+"/api/v1/admin/campaigns", `{"name":"p3","commission_months":12}`, f.adminKey)
	if code != 201 || out["commission_months"].(float64) != 12 {
		t.Fatalf("create with cap: %d %v", code, out)
	}
	code, out = patch(fmt.Sprintf("/api/v1/admin/campaigns/%d", id), `{"commission_months":6}`)
	if code != 200 || out["commission_months"].(float64) != 6 || out["net_terms_days"].(float64) != 45 {
		t.Fatalf("patch cap: %d %v", code, out)
	}
	for _, body := range []string{`{"commission_months":-1}`, `{"commission_months":121}`} {
		if code, _ := patch(fmt.Sprintf("/api/v1/admin/campaigns/%d", id), body); code != 400 {
			t.Fatalf("patch %s: %d want 400", body, code)
		}
	}
	if code, _ := paPost(t, f.srv.URL+"/api/v1/admin/campaigns", `{"name":"p4","commission_months":500}`, f.adminKey); code != 400 {
		t.Fatalf("create with cap 500: %d want 400", code)
	}
	_, out = getJSON(t, f.srv.URL+"/api/v1/admin/campaigns", f.adminKey)
	for _, c := range out["campaigns"].([]any) {
		m := c.(map[string]any)
		if int64(m["id"].(float64)) == id && m["commission_months"].(float64) != 6 {
			t.Fatalf("campaign list commission_months: %v", m)
		}
	}
}

func jsonDecode(res *http.Response, v any) error { return json.NewDecoder(res.Body).Decode(v) }
