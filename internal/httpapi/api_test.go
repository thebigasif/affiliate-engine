package httpapi

// API surface tests (FR-27/28/29): auth enforcement, scope isolation,
// money numbers derived from ledger/commissions only, rate limiting.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// apiFixture is one migrated file-backed DB with two affiliates, posted
// commissions through the real ledger path, and an API server + keys.
type apiFixture struct {
	srv      *httptest.Server
	db       *db.Handles
	adminKey string
	aff1Key  string
	aff2Key  string
}

func setupAPI(t *testing.T) *apiFixture {
	t.Helper()
	return setupAPILimit(t, 0)
}

func setupAPILimit(t *testing.T, rateLimit int) *apiFixture {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)

	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"AFFILIATE_PAYABLE:2:USD": "liability",
		"PAYOUT_CLEARING:USD":     "clearing",
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (?, ?)`,
			id, fmt.Sprintf("aff%d@x.test", id)); err != nil {
			t.Fatal(err)
		}
	}

	// Real commission, posted where the engine keeps its money: a pending
	// commission sits in HOLD (it reaches the payable only at maturation),
	// an approved or paid one has moved to the payable (credit = negative).
	// Returns the commission id.
	postCommission := func(affID int64, amount int64, status string) int64 {
		t.Helper()
		account := fmt.Sprintf("AFFILIATE_PAYABLE:%d:USD", affID)
		if status == "pending" {
			account = "HOLD:USD"
		}
		jid, err := poster.Post(ctx, fmt.Sprintf("test:comm:%d:%d:%s", affID, amount, status),
			"conversion", "test commission", time.Now().UTC(), []ledger.Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: amount},
				{Account: account, Amount: -amount},
			})
		if err != nil {
			t.Fatal(err)
		}
		var cid int64
		if err := h.DB.QueryRow(`
			INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency,
			                         rate_snapshot, status, matures_at, created_at)
			VALUES (?, 1, ?, ?, 'USD', '{}', ?, ?, ?) RETURNING id`,
			affID, jid, amount, status,
			time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339),
			time.Now().UTC().Format(time.RFC3339)).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		return cid
	}
	postCommission(1, 2000, "pending")
	postCommission(1, 3000, "approved")
	paidCommID := postCommission(1, 5000, "paid")
	postCommission(2, 7000, "pending")

	// The paid commission settled through a payout journal (payable ->
	// clearing), exactly as the payout runner does. Without this the fixture
	// would claim a paid commission the ledger never paid out.
	var payoutID int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount, currency, method, status, processed_at)
		VALUES (1, 5000, 'USD', 'manual', 'paid', ?) RETURNING id`,
		time.Now().UTC().Format(time.RFC3339)).Scan(&payoutID); err != nil {
		t.Fatal(err)
	} else {
		if _, err := h.DB.Exec(`INSERT INTO payout_lines (payout_id, commission_id) VALUES (?, ?)`,
			payoutID, paidCommID); err != nil {
			t.Fatal(err)
		}
		if _, err := poster.Post(ctx, fmt.Sprintf("test:payout:%d", payoutID), "payout", "test payout",
			time.Now().UTC(), []ledger.Line{
				{Account: "AFFILIATE_PAYABLE:1:USD", Amount: 5000},
				{Account: "PAYOUT_CLEARING:USD", Amount: -5000},
			}); err != nil {
			t.Fatal(err)
		}
	}

	// daily_agg rows for clicks (today).
	day := time.Now().UTC().Format("20060102")
	for _, r := range []struct{ aff, clicks int64 }{{1, 100}, {2, 50}} {
		if _, err := h.DB.Exec(`
			INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks, conversions, commission_cents)
			VALUES (?, ?, 1, ?, 0, 0)`, day, r.aff, r.clicks); err != nil {
			t.Fatal(err)
		}
	}

	f := &apiFixture{db: h}
	var err error
	if f.adminKey, err = apikeys.Store(ctx, h.DB, "admin test", apikeys.ScopeAdmin); err != nil {
		t.Fatal(err)
	}
	if f.aff1Key, err = apikeys.Store(ctx, h.DB, "aff1 test", apikeys.AffiliateScope(1)); err != nil {
		t.Fatal(err)
	}
	if f.aff2Key, err = apikeys.Store(ctx, h.DB, "aff2 test", apikeys.AffiliateScope(2)); err != nil {
		t.Fatal(err)
	}

	// Payout runner wired with the ledger poster (same shape as main/e2e).
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := poster.Post(ctx, fmt.Sprintf("payout:%d", payoutID), "payout", "api test",
			time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = poster
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })

	f.srv = httptest.NewServer(NewAPIHandler(APIDeps{
		DB:           h.DB,
		PayoutRunner: payouts.NewRunner(h.DB),
		RateLimit:    rateLimit,
		TaxDocKey:    DeriveTaxDocKey([]byte("api-test-deployment-secret-0123456789")),
		SiteFetch:    http.DefaultClient, // test sites are on localhost
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func getJSON(t *testing.T, url, key string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, m
}

func TestAPI_AuthRequired(t *testing.T) {
	f := setupAPI(t)
	for _, path := range []string{
		"/api/v1/affiliate/earnings",
		"/api/v1/affiliate/ledger",
		"/api/v1/admin/dashboard",
		"/api/v1/admin/affiliates",
		"/api/v1/admin/fraud/queue",
	} {
		code, m := getJSON(t, f.srv.URL+path, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("%s: code=%d want 401", path, code)
		}
		if m["error"] == nil {
			t.Fatalf("%s: no error body", path)
		}
	}
}

func TestAPI_BadKeyRejected(t *testing.T) {
	f := setupAPI(t)
	code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", "ae_totallymadeup")
	if code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", code)
	}
}

func TestAPI_RevokedKeyRejected(t *testing.T) {
	ctx := context.Background()
	f := setupAPI(t)
	if _, err := f.db.DB.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE prefix = ?`,
		time.Now().UTC().Format(time.RFC3339), f.aff1Key[:10]); err != nil {
		t.Fatal(err)
	}
	code, _ := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key)
	if code != http.StatusUnauthorized {
		t.Fatalf("revoked key code=%d want 401", code)
	}
}

func TestAPI_ScopeIsolation(t *testing.T) {
	f := setupAPI(t)

	// Affiliate key cannot reach admin routes.
	if code, _ := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.aff1Key); code != http.StatusForbidden {
		t.Fatalf("aff key on admin route: code=%d want 403", code)
	}

	// Affiliate 2's key sees ONLY affiliate 2's money (7000 pending),
	// never affiliate 1's.
	code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff2Key)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got := m["unpaid_cents"].(float64); got != 7000 {
		t.Fatalf("aff2 unpaid=%v want 7000 (isolation broken?)", got)
	}
	if got := m["affiliate_id"].(float64); got != 2 {
		t.Fatalf("affiliate_id=%v want 2", got)
	}
}

func TestAPI_EarningsNumbersAndProof(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	// unpaid = pending(2000) + approved(3000) = 5000; only the approved
	// 3000 is in the payable (pending sits in HOLD): owed 3000 == -payable.
	if got := m["unpaid_cents"].(float64); got != 5000 {
		t.Fatalf("unpaid=%v want 5000", got)
	}
	if got := m["payable_balance_cents"].(float64); got != -3000 {
		t.Fatalf("payable=%v want -3000", got)
	}
	proof := m["ledger_proof"].(map[string]any)
	if proof["holds"] != true || proof["owed_cents"].(float64) != 3000 {
		t.Fatalf("ledger proof failed: %v", proof)
	}
	if got := m["ctr_30d"].(float64); got != 0 { // fixture: 0 conversions in agg
		t.Fatalf("ctr=%v want 0 (no conversions in daily_agg fixture)", got)
	}
	if got := m["clicks_30d"].(float64); got != 100 {
		t.Fatalf("clicks=%v want 100", got)
	}
	totals := m["totals"].(map[string]any)
	if totals["paid"].(map[string]any)["cents"] != float64(5000) {
		t.Fatalf("paid total=%v", totals["paid"])
	}
	recent := m["recent_commissions"].([]any)
	if len(recent) != 3 {
		t.Fatalf("recent=%d want 3", len(recent))
	}
}

func TestAPI_LedgerViewSumZero(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/ledger", f.aff1Key)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	entries := m["entries"].([]any)
	if len(entries) != 3 { // approved + paid commissions + 1 payout settlement (pending is in HOLD)
		t.Fatalf("entries=%d want 3", len(entries))
	}
	proof := m["proof"].(map[string]any)
	if proof["all_journals_sum_zero"] != true {
		t.Fatal("a journal does not sum to zero — integrity violation surfaced")
	}
	for _, e := range entries {
		em := e.(map[string]any)
		if got := em["journal_sum_cents"].(float64); got != 0 {
			t.Fatalf("journal %v sum=%v want 0", em["journal_id"], got)
		}
		if got := em["journal_lines"].(float64); got < 2 {
			t.Fatalf("journal %v lines=%v want >=2", em["journal_id"], got)
		}
	}
	if got := m["page_sum_cents"].(float64); got != -3000 {
		t.Fatalf("page_sum=%v want -3000 (8000 matured - 5000 paid out)", got)
	}
	if m["next_cursor"].(float64) != 0 {
		t.Fatalf("next_cursor=%v want 0 (all 3 entries fit one page)", m["next_cursor"])
	}

	// Pagination drill: limit=2 -> a page of 2, then a last page of 1.
	code, m = getJSON(t, f.srv.URL+"/api/v1/affiliate/ledger?limit=2", f.aff1Key)
	if code != http.StatusOK || len(m["entries"].([]any)) != 2 {
		t.Fatalf("page1: code=%d entries=%v", code, m["entries"])
	}
	cur := m["next_cursor"].(float64)
	if cur == 0 {
		t.Fatal("page1 must yield a cursor")
	}
	code, m = getJSON(t, f.srv.URL+fmt.Sprintf("/api/v1/affiliate/ledger?limit=2&cursor=%d", int64(cur)), f.aff1Key)
	if code != http.StatusOK || len(m["entries"].([]any)) != 1 {
		t.Fatalf("page2: code=%d entries=%v", code, m["entries"])
	}
	if m["next_cursor"].(float64) != 0 {
		t.Fatal("page2 must be the last page")
	}
}

func TestAPI_AdminDashboard(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.adminKey)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got := m["ledger_integrity_sum"].(float64); got != 0 {
		t.Fatalf("ledger sum=%v want 0", got)
	}
	if got := m["clicks_total"].(float64); got != 150 {
		t.Fatalf("clicks_total=%v want 150", got)
	}
	comm := m["commissions_by_status_cents"].(map[string]any)
	if got := comm["pending"].(float64); got != 9000 {
		t.Fatalf("pending total=%v want 9000 (2000+7000)", got)
	}
	leaders := m["top_affiliates"].([]any)
	if len(leaders) != 2 {
		t.Fatalf("leaders=%d want 2", len(leaders))
	}
}

func TestAPI_AdminAffiliatesList(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/admin/affiliates", f.adminKey)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	affs := m["affiliates"].([]any)
	if len(affs) != 2 {
		t.Fatalf("affiliates=%d want 2", len(affs))
	}
	first := affs[0].(map[string]any)
	if got := first["pending_cents"].(float64); got != 2000 {
		t.Fatalf("aff1 pending=%v want 2000", got)
	}
	if got := first["clicks_30d"].(float64); got != 100 {
		t.Fatalf("aff1 clicks=%v want 100", got)
	}
}

func TestAPI_FraudQueueEmptyOK(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/admin/fraud/queue", f.adminKey)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if m["verdicts"] == nil || m["holds"] == nil {
		t.Fatalf("missing keys: %v", m)
	}
}

func TestAPI_RateLimitEnforced(t *testing.T) {
	f := setupAPILimit(t, 3)
	var last int
	for i := 0; i < 5; i++ {
		last, _ = getJSON(t, f.srv.URL+"/api/v1/admin/dashboard", f.adminKey)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("5th request code=%d want 429", last)
	}
}

func TestAPI_AdminNeedsAffiliateIDParam(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings", f.adminKey)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
	if !strings.Contains(m["error"].(string), "affiliate_id") {
		t.Fatalf("error=%v", m["error"])
	}
}

func TestAPI_AdminCanReadAffiliateView(t *testing.T) {
	f := setupAPI(t)
	code, m := getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings?affiliate_id=1", f.adminKey)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got := m["affiliate_id"].(float64); got != 1 {
		t.Fatalf("affiliate_id=%v want 1", got)
	}
	// Admin asking for a bad id: 400.
	if code, _ = getJSON(t, f.srv.URL+"/api/v1/affiliate/earnings?affiliate_id=abc", f.adminKey); code != http.StatusBadRequest {
		t.Fatalf("bad affiliate_id code=%d want 400", code)
	}
}

func TestAPI_PayoutRunDryRun(t *testing.T) {
	f := setupAPI(t)
	body := strings.NewReader(`{"min_threshold_cents": 1000, "dry_run": true}`)
	req, err := http.NewRequest("POST", f.srv.URL+"/api/v1/admin/payouts/run", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d body=%v", resp.StatusCode, m)
	}
	if m["dry_run"] != true {
		t.Fatalf("dry_run=%v", m["dry_run"])
	}
	// Only affiliate 1 has approved commissions (3000); aff2 has none approved.
	batches := m["batches"].([]any)
	if len(batches) != 1 {
		t.Fatalf("batches=%d want 1 (only aff1 approved): %v", len(batches), batches)
	}
	b := batches[0].(map[string]any)
	if b["affiliate_id"].(float64) != 1 || b["cents"].(float64) != 3000 {
		t.Fatalf("batch=%v", b)
	}
}

func TestAPI_PayoutRunDeniedWithoutAdmin(t *testing.T) {
	f := setupAPI(t)
	body := strings.NewReader(`{}`)
	req, _ := http.NewRequest("POST", f.srv.URL+"/api/v1/admin/payouts/run", body)
	req.Header.Set("Authorization", "Bearer "+f.aff1Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("code=%d want 403", resp.StatusCode)
	}
}
