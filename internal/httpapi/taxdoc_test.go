package httpapi_test

// FR-17 tax-doc endpoints (dual-engine): admin CRUD on /admin/taxdocs,
// the dry-run payout preview mirroring the gate, and the affiliate-side
// earnings visibility field. Runs on SQLite and PG via dbtest.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

type taxdocFixture struct {
	srv      *httptest.Server
	adminKey string
	aff1Key  string
}

func setupTaxdoc(t *testing.T) *taxdocFixture {
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
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1,'t1@x.test'), (2,'t2@x.test')`); err != nil {
		t.Fatal(err)
	}
	// One approved, matured commission per affiliate (real journals, real
	// ledger postings — the payout query joins payout_lines/holds).
	for _, aff := range []int64{1, 2} {
		jid, err := poster.Post(ctx, fmt.Sprintf("taxdoc:comm:%d", aff), "conversion",
			"taxdoc test", time.Now().UTC(), []ledger.Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: 10000},
				{Account: fmt.Sprintf("AFFILIATE_PAYABLE:%d:USD", aff), Amount: -10000},
			})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions
			(affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot,
			 status, matures_at, approved_at, created_at)
			VALUES (?, 1, ?, 10000, 'USD', '{}', 'approved', ?, ?, ?)`,
			aff, jid,
			time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
			time.Now().UTC().Format(time.RFC3339Nano),
			time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	adminKey, err := apikeys.Store(ctx, h.DB, "taxdoc admin", apikeys.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	aff1Key, err := apikeys.Store(ctx, h.DB, "taxdoc aff1", apikeys.AffiliateScope(1))
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpapi.NewAPIHandler(httpapi.APIDeps{
		DB:           h.DB,
		PayoutRunner: payouts.NewRunner(h.DB),
	}))
	t.Cleanup(srv.Close)
	return &taxdocFixture{srv: srv, adminKey: adminKey, aff1Key: aff1Key}
}

func taxdocDo(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequest(method, url, nil)
	} else {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s: %v (status %d)", method, url, err, res.StatusCode)
	}
	return res.StatusCode, out
}

// TX1: record -> list -> upsert lifecycle; reviewed_at stamped only on
// operator review, never on 'pending'.
func TestTaxDocs_RecordAndList(t *testing.T) {
	f := setupTaxdoc(t)
	base := f.srv.URL + "/api/v1/admin/taxdocs"

	// No docs yet: empty list.
	code, out := taxdocDo(t, "GET", base, f.adminKey, "")
	if code != 200 {
		t.Fatalf("list: %d %v", code, out)
	}

	// Record a pending W-9 for affiliate 1.
	code, out = taxdocDo(t, "POST", base, f.adminKey,
		`{"affiliate_id":1,"doc_type":"w9","status":"pending","reference":"IRS-1"}`)
	if code != 201 {
		t.Fatalf("record: %d %v", code, out)
	}

	// Upsert to received (operator looked at it).
	code, _ = taxdocDo(t, "POST", base, f.adminKey,
		`{"affiliate_id":1,"doc_type":"w9","status":"received","reference":"IRS-1"}`)
	if code != 201 {
		t.Fatalf("upsert: %d", code)
	}

	code, out = taxdocDo(t, "GET", base+"?affiliate_id=1", f.adminKey, "")
	if code != 200 {
		t.Fatalf("list aff1: %d %v", code, out)
	}
	docs, _ := out["tax_documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("docs=%d want 1 (upsert must not duplicate)", len(docs))
	}
	d := docs[0].(map[string]any)
	if d["status"] != "received" {
		t.Fatalf("status=%v want received", d["status"])
	}
	if ra, _ := d["reviewed_at"].(string); ra == "" {
		t.Fatal("reviewed_at must be stamped on review statuses")
	}

	// Back to pending re-opens the requirement and clears the stamp.
	if code, _ := taxdocDo(t, "POST", base, f.adminKey,
		`{"affiliate_id":1,"doc_type":"w9","status":"pending"}`); code != 201 {
		t.Fatalf("reopen: %d", code)
	}
	_, out = taxdocDo(t, "GET", base+"?affiliate_id=1", f.adminKey, "")
	docs, _ = out["tax_documents"].([]any)
	d = docs[0].(map[string]any)
	if ra, _ := d["reviewed_at"].(string); ra != "" {
		t.Fatalf("reviewed_at=%q want empty for pending", ra)
	}

	// Validation: bad doc_type, bad status, unknown affiliate, bad id.
	for _, body := range []string{
		`{"affiliate_id":1,"doc_type":"passport","status":"pending"}`,
		`{"affiliate_id":1,"doc_type":"w9","status":"garbage"}`,
		`{"affiliate_id":999,"doc_type":"w9","status":"pending"}`,
		`{"affiliate_id":0,"doc_type":"w9","status":"pending"}`,
	} {
		if code, _ := taxdocDo(t, "POST", base, f.adminKey, body); code != 400 {
			t.Fatalf("validation: %s -> %d want 400", body, code)
		}
	}
	// Non-admin key: 403.
	if code, _ := taxdocDo(t, "GET", base, f.aff1Key, ""); code != 403 {
		t.Fatalf("affiliate key on admin surface: %d want 403", code)
	}
}

// TX2: the dry-run preview mirrors the real gate — the blocked affiliate
// never appears in batches, and the real run stages only the clean one.
func TestTaxDocs_PayoutRunMirror(t *testing.T) {
	f := setupTaxdoc(t)
	run := f.srv.URL + "/api/v1/admin/payouts/run"

	// Block affiliate 1.
	code, out := taxdocDo(t, "POST", f.srv.URL+"/api/v1/admin/taxdocs", f.adminKey,
		`{"affiliate_id":1,"doc_type":"w9","status":"pending"}`)
	if code != 201 {
		t.Fatalf("record: %d %v", code, out)
	}

	// Dry run: only affiliate 2's commission is eligible.
	code, out = taxdocDo(t, "POST", run, f.adminKey,
		`{"min_threshold_cents":0,"method":"manual","dry_run":true}`)
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	batches, _ := out["batches"].([]any)
	if len(batches) != 1 {
		t.Fatalf("dry-run batches=%d want 1 (affiliate 1 tax-blocked)", len(batches))
	}
	b := batches[0].(map[string]any)
	if b["affiliate_id"] != float64(2) {
		t.Fatalf("batch affiliate=%v want 2", b["affiliate_id"])
	}

	// Real run agrees with the preview.
	code, out = taxdocDo(t, "POST", run, f.adminKey,
		`{"min_threshold_cents":0,"method":"manual"}`)
	if code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	if n, _ := out["payouts_created"].(float64); n != 1 {
		t.Fatalf("payouts_created=%v want 1", out["payouts_created"])
	}
	// The clean affiliate got the payout; verify via a second dry-run:
	// affiliate 2 is claimed and affiliate 1 still tax-blocked, so
	// nothing remains eligible.
	code, out = taxdocDo(t, "POST", run, f.adminKey,
		`{"min_threshold_cents":0,"method":"manual","dry_run":true}`)
	if code != 200 {
		t.Fatalf("dry run 2: %d %v", code, out)
	}
	batches, _ = out["batches"].([]any)
	if len(batches) != 0 {
		t.Fatalf("post-run dry-run batches=%d want 0 (aff2 claimed, aff1 blocked)", len(batches))
	}
}

// TX3: affiliate-side visibility — earnings tells the blocked affiliate
// WHY their approved balance is not batching.
func TestTaxDocs_EarningsVisibility(t *testing.T) {
	f := setupTaxdoc(t)

	// Nothing recorded: field present and empty.
	code, out := taxdocDo(t, "GET", f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key, "")
	if code != 200 {
		t.Fatalf("earnings: %d %v", code, out)
	}
	if v, ok := out["payout_blocked_by_taxdoc"]; !ok || v != "" {
		t.Fatalf("blocked field=%v (want present, empty)", out["payout_blocked_by_taxdoc"])
	}

	// Record a pending doc: field names the doc type.
	if code, _ := taxdocDo(t, "POST", f.srv.URL+"/api/v1/admin/taxdocs", f.adminKey,
		`{"affiliate_id":1,"doc_type":"w8bene","status":"pending"}`); code != 201 {
		t.Fatalf("record: %d", code)
	}
	code, out = taxdocDo(t, "GET", f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key, "")
	if code != 200 {
		t.Fatalf("earnings 2: %d %v", code, out)
	}
	if v, _ := out["payout_blocked_by_taxdoc"].(string); v != "w8bene" {
		t.Fatalf("blocked field=%q want w8bene", v)
	}

	// Validated: block lifts.
	if code, _ := taxdocDo(t, "POST", f.srv.URL+"/api/v1/admin/taxdocs", f.adminKey,
		`{"affiliate_id":1,"doc_type":"w8bene","status":"validated"}`); code != 201 {
		t.Fatalf("validate: %d", code)
	}
	code, out = taxdocDo(t, "GET", f.srv.URL+"/api/v1/affiliate/earnings", f.aff1Key, "")
	if code != 200 {
		t.Fatalf("earnings 3: %d", code)
	}
	if v, _ := out["payout_blocked_by_taxdoc"].(string); v != "" {
		t.Fatalf("blocked field=%q want empty after validation", v)
	}
}
