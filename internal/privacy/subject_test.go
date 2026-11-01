package privacy_test

// NFR-7 subject-rights tests (dual-engine: SQLite file-backed + PG when
// AE_TEST_ENGINES/DSN are configured). The erasure contract under test:
//
//  1. every identity field attributable to the subject is scrubbed or
//     deleted across ALL PII-bearing tables (the fixture seeds every one),
//  2. money rows survive intact — commissions keep amount/status, the
//     ledger still sums to zero (the books stay balanced),
//  3. a control affiliate is untouched (no cross-subject blast radius),
//  4. erase is idempotent — the second run reports zero rows,
//  5. access dies: sessions/grants/API keys revoked,
//  6. the erasure itself lands in the append-only audit_log.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/privacy"
)

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("seed: %s: %v", q, err)
	}
}

func oneInt(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func oneStr(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

// seedWorld builds a full-PII fixture around affiliate 1 (the subject)
// and affiliate 2 (the control). Returns the migrated handles.
func seedWorld(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)

	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"AFFILIATE_PAYABLE:2:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}

	mustExec(t, h.DB, `INSERT INTO affiliates (id, email, name, status, postback_url, postback_secret)
		VALUES (1, 'subject@x.test', 'Subject One', 'active', 'https://pb.x.test/cb?click={click_id}', 'sec_sec_sec'),
		       (2, 'control@x.test', 'Control Two', 'active', '', '')`)

	// Pre-identity application row (keyed by email).
	mustExec(t, h.DB, `INSERT INTO affiliate_applications (email, name, note, status)
		VALUES ('subject@x.test', 'Subject One', 'please', 'approved')`)

	// Access surfaces.
	mustExec(t, h.DB, `INSERT INTO affiliate_sessions (token_hash, affiliate_id, expires_at)
		VALUES ('aa', 1, '2030-01-01T00:00:00Z'), ('bb', 2, '2030-01-01T00:00:00Z')`)
	mustExec(t, h.DB, `INSERT INTO portal_login_grants (token_hash, affiliate_id, expires_at)
		VALUES ('gg', 1, '2030-01-01T00:00:00Z')`)

	// Clicks + derived counters + attribution + customer binding.
	// (Blobs go through parameters: SQLite's X'..' hex literal is not
	// portable — PG parses it as a bit constant.)
	ipA := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	uaA := []byte{17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	ipB := []byte{33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48}
	mustExec(t, h.DB, `INSERT INTO clicks (id, ts, day, month, affiliate_id, ip_hash, ua_hash,
		referer, landing_path, utm_source, subid, placement, extra_json, country)
		VALUES ('clk1', '2026-09-01T00:00:00Z', 20260901, 202609, 1, ?, ?,
			'https://google.com/q', '/pricing', 'nl', 's1', 'hero', '{"campaign":"fall"}', 'DE')`, ipA, uaA)
	mustExec(t, h.DB, `INSERT INTO clicks (id, ts, day, month, affiliate_id, ip_hash, ua_hash,
		referer, landing_path, utm_source, subid, placement, extra_json, country)
		VALUES ('clk2', '2026-09-01T01:00:00Z', 20260901, 202609, 2, ?, NULL,
			'', '/', '', '', '', '{}', 'US')`, ipB)
	mustExec(t, h.DB, `INSERT INTO uniq_daily (day, affiliate_id, ip_hash)
		VALUES (20260901, 1, ?), (20260901, 2, ?)`, ipA, ipB)
	mustExec(t, h.DB, `INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
		VALUES ('clk1', 1, 1, 'token', '2026-09-01T00:00:00Z', '2026-10-01T00:00:00Z'),
		       ('clk2', 2, 1, 'token', '2026-09-01T01:00:00Z', '2026-10-01T00:00:00Z')`)
	mustExec(t, h.DB, `INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_at, bound_method)
		VALUES (1, 'cus_subj', 1, '2026-09-01T00:00:00Z', 'token'),
		       (1, 'cus_ctrl', 2, '2026-09-01T01:00:00Z', 'token')`)

	// Money: one commission each, real journal postings.
	for _, aff := range []int64{1, 2} {
		jid, err := poster.Post(ctx, fmt.Sprintf("priv:comm:%d", aff), "conversion",
			"privacy test", time.Now().UTC(), []ledger.Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: 5000},
				{Account: fmt.Sprintf("AFFILIATE_PAYABLE:%d:USD", aff), Amount: -5000},
			})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, h.DB, `INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency,
			rate_snapshot, status, matures_at, buyer_email, attribution_method, coupon_code, country,
			customer_ext_id, click_id, order_id, gross_minor, created_at)
			VALUES (?, 1, ?, 5000, 'USD', '{}', 'pending', '2026-10-01T00:00:00Z', ?, 'token', ?, 'DE', ?, ?, ?, 20000, '2026-09-01T00:00:00Z')`,
			aff, jid,
			fmt.Sprintf("buyer%d@shop.test", aff),
			fmt.Sprintf("SAVE%d", aff),
			fmt.Sprintf("cus_%d", aff),
			fmt.Sprintf("clk%d", aff),
			fmt.Sprintf("ord_%d", aff))
	}
	// Commission hold (fraud enforcement) on the subject's commission.
	mustExec(t, h.DB, `INSERT INTO commission_holds (commission_id, reason, note)
		VALUES (1, 'self-referral: subject@x.test vs buyer1@shop.test', 'operator note')`)

	// Payout.
	mustExec(t, h.DB, `INSERT INTO payouts (id, affiliate_id, amount, currency, method, status, provider_ref)
		VALUES (10, 1, 5000, 'USD', 'paypal', 'requested', 'PPI-xyz')`)

	// Link identity. (is_primary/active are BOOLEAN on PG, INTEGER on
	// SQLite — parameters keep the seed dual-engine.)
	mustExec(t, h.DB, `INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source)
		VALUES (1, 'subject', ?, 'native'), (2, 'control', ?, 'native')`, true, true)
	mustExec(t, h.DB, `INSERT INTO coupons (code, affiliate_id, campaign_id, rate_type, rate_value)
		VALUES ('savesubj', 1, 1, 'percent', 1000), ('savectrl', 2, 1, 'percent', 1000)`)

	// Gamification.
	mustExec(t, h.DB, `INSERT INTO badges (code, name) VALUES ('first_commission', 'First')`)
	mustExec(t, h.DB, `INSERT INTO xp_events (affiliate_id, reason, xp, source_ref)
		VALUES (1, 'conversion', 10, '1'), (2, 'conversion', 10, '2')`)
	mustExec(t, h.DB, `INSERT INTO xp_totals (affiliate_id, xp, level) VALUES (1, 10, 2), (2, 10, 2)`)
	mustExec(t, h.DB, `INSERT INTO affiliate_badges (affiliate_id, badge_code)
		VALUES (1, 'first_commission'), (2, 'first_commission')`)

	// Tax documents.
	mustExec(t, h.DB, `INSERT INTO tax_documents (affiliate_id, doc_type, status, reference, note)
		VALUES (1, 'w9', 'received', 'collector-subject-1', 'Subject One form'),
		       (2, 'w9', 'received', 'collector-control-2', 'Control Two form')`)
	// Uploaded form files (sealed PDFs carrying the TIN).
	mustExec(t, h.DB, `INSERT INTO tax_document_files (tax_document_id, affiliate_id, file_name, size_bytes, sha256, sealed)
		SELECT id, affiliate_id, 'w9.pdf', 3, '00', 'x' FROM tax_documents`)

	// Fraud verdicts — BOTH subject formats the ruleset emits.
	mustExec(t, h.DB, `INSERT INTO fraud_verdicts (rule, subject, reason, action) VALUES
		('duplicate_account', '1', 'alias of subject@x.test', 1),
		('ip_overlap', 'affiliate:1', 'shares /24 with subject@x.test', 1),
		('click_velocity', 'ip:abcd', 'unrelated verdict stays', 1),
		('ctr_30d', '2', 'control affiliate verdict', 1)`)

	// The leak shapes the first erasure pass missed (found by residue
	// review): verdicts on OTHER affiliates quoting the subject's email
	// (duplicate_account fires per-account over a shared alias family),
	// and coupon_hijack verdicts keyed by the subject's coupon code.
	mustExec(t, h.DB, `INSERT INTO fraud_verdicts (rule, subject, reason, action) VALUES
		('duplicate_account', '2', 'email "subject@x.test" (alias-normalized) is used by 1 other active accounts', 1),
		('duplicate_account', 'affiliate:2', 'email "subject+x@x.test" (alias-normalized) is used by 1 other active accounts', 1),
		('coupon_hijack', 'savesubj', 'coupon "savesubj" overrode 12 existing customer bindings in 7d', 1)`)

	// Import undo record embedding the subject's email among others.
	undo := `{"source":"rewardful","affiliates":[` +
		`{"affiliate_id":1,"email":"subject@x.test","code":"subject"},` +
		`{"affiliate_id":2,"email":"control@x.test","code":"control"}]}`
	mustExec(t, h.DB, `INSERT INTO import_batches (id, source, dry_run, affiliates) VALUES (7, 'rewardful', ?, 2)`, false)
	mustExec(t, h.DB, `INSERT INTO import_undos (batch_id, undo_json) VALUES (7, ?)`, undo)

	// Affiliate-scoped + admin API keys.
	mustExec(t, h.DB, `INSERT INTO api_keys (name, key_hash, prefix, scope)
		VALUES ('subj key', 'kh1', 'ae_SUBJ', 'affiliate:1'),
		       ('ctrl key', 'kh2', 'ae_CTRL', 'affiliate:2'),
		       ('admin key', 'kh3', 'ae_ADMIN', 'admin')`)

	return h.DB
}

// TestPrivacy_ExportFullCoverage verifies the Art. 15/20 bundle collects
// every PII-bearing surface the fixture seeds, with correct counts.
func TestPrivacy_ExportFullCoverage(t *testing.T) {
	db := seedWorld(t)
	s := privacy.NewStore(db)

	b, err := s.Export(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.Subject["email"] != "subject@x.test" {
		t.Fatalf("subject email = %v", b.Subject["email"])
	}
	if b.Application == nil || b.Application["note"] != "please" {
		t.Fatalf("application row missing: %v", b.Application)
	}
	want := map[string]int64{
		"clicks": 1, "attributions": 1, "customers": 1, "commissions": 1,
		"payouts": 1, "codes": 1, "coupons": 1,
	}
	for k, v := range want {
		if b.Counters[k] != v {
			t.Errorf("counter %s = %d want %d", k, b.Counters[k], v)
		}
	}
	if len(b.Badges) != 1 || len(b.TaxDocuments) != 1 {
		t.Errorf("badges=%d taxdocs=%d want 1/1", len(b.Badges), len(b.TaxDocuments))
	}
	if b.XPTotal == nil || b.XPTotal["xp"] != int64(10) {
		t.Errorf("xp total = %v", b.XPTotal)
	}
	if b.Commissions[0]["buyer_email"] != "buyer1@shop.test" {
		t.Errorf("commission buyer_email = %v", b.Commissions[0]["buyer_email"])
	}
	js, err := b.JSON()
	if err != nil || len(js) == 0 {
		t.Fatalf("JSON render: %v", err)
	}

	// Unknown subject is an explicit error, not an empty bundle.
	if _, err := s.Export(context.Background(), 999); err != privacy.ErrUnknownAffiliate {
		t.Fatalf("unknown affiliate err = %v", err)
	}
}

// TestPrivacy_ExportPayoutDetailsAndFraud: the bundle carries the
// affiliate's payout details (unsealed when a key is present) and the
// fraud records about them, and nothing about the control affiliate.
func TestPrivacy_ExportPayoutDetailsAndFraud(t *testing.T) {
	db := seedWorld(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO affiliate_payout_details (affiliate_id, method, sealed) VALUES (1, 'paypal', ?), (2, 'wise', ?)`,
		[]byte("sealed-1"), []byte("sealed-2"))

	// Without an opener the row is listed, the fields stay sealed.
	b, err := privacy.NewStore(db).Export(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.PayoutDetails == nil || b.PayoutDetails["method"] != "paypal" || b.PayoutDetails["details_readable"] != false {
		t.Fatalf("sealed payout details = %v", b.PayoutDetails)
	}
	if _, ok := b.PayoutDetails["paypal_email"]; ok {
		t.Fatalf("plain field present without a key: %v", b.PayoutDetails)
	}

	opened := 0
	open := func(affID int64, sealed []byte) (map[string]any, bool) {
		opened++
		if affID != 1 || string(sealed) != "sealed-1" {
			return nil, false
		}
		return map[string]any{"method": "paypal", "paypal_email": "pay@subject.test"}, true
	}
	b, err = privacy.NewStore(db).WithPayoutOpener(open).Export(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 || b.PayoutDetails["paypal_email"] != "pay@subject.test" || b.PayoutDetails["details_readable"] != true {
		t.Fatalf("opened payout details = %v (opened %d)", b.PayoutDetails, opened)
	}

	// A row that does not open is listed as not readable.
	if _, err := db.Exec(`UPDATE affiliate_payout_details SET sealed = ? WHERE affiliate_id = 1`, []byte("damaged")); err != nil {
		t.Fatal(err)
	}
	b, err = privacy.NewStore(db).WithPayoutOpener(open).Export(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.PayoutDetails["details_readable"] != false {
		t.Fatalf("damaged row = %v", b.PayoutDetails)
	}

	// Fraud: the verdicts keyed by the subject's id forms and coupon,
	// and the hold on their commission. Verdicts about the control
	// affiliate (even ones quoting the subject's email) stay out.
	if len(b.FraudVerdicts) != 3 || b.Counters["fraud_verdicts"] != 3 {
		t.Fatalf("fraud verdicts = %v", b.FraudVerdicts)
	}
	subjects := map[string]bool{}
	for _, v := range b.FraudVerdicts {
		subjects[fmt.Sprint(v["subject"])] = true
	}
	for _, want := range []string{"1", "affiliate:1", "savesubj"} {
		if !subjects[want] {
			t.Errorf("verdict with subject %q missing: %v", want, b.FraudVerdicts)
		}
	}
	if len(b.FraudHolds) != 1 || b.Counters["fraud_holds"] != 1 {
		t.Fatalf("fraud holds = %v", b.FraudHolds)
	}
	if b.FraudHolds[0]["note"] != "operator note" || !containsAny(fmt.Sprint(b.FraudHolds[0]["reason"]), "self-referral") {
		t.Errorf("hold = %v", b.FraudHolds[0])
	}

	// An affiliate without details or fraud records exports cleanly.
	mustExec(t, db, `INSERT INTO affiliates (id, email, name) VALUES (3, 'clean@x.test', 'Clean')`)
	b, err = privacy.NewStore(db).WithPayoutOpener(open).Export(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if b.PayoutDetails != nil || len(b.FraudVerdicts) != 0 || len(b.FraudHolds) != 0 {
		t.Fatalf("clean affiliate bundle: %v %v %v", b.PayoutDetails, b.FraudVerdicts, b.FraudHolds)
	}
}

// TestPrivacy_EraseScrubsEverything is the core Art. 17 contract: no
// stored string may still identify the subject afterwards, while money
// rows and the control affiliate survive untouched.
func TestPrivacy_EraseScrubsEverything(t *testing.T) {
	db := seedWorld(t)
	s := privacy.NewStore(db)
	ctx := context.Background()

	// Pre-erase: the money baseline.
	sumZero := oneInt(t, db, `SELECT COALESCE(SUM(amount),0) FROM ledger_entries`)
	payable1 := oneInt(t, db, `SELECT COALESCE(SUM(amount),0) FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id WHERE la.code = 'AFFILIATE_PAYABLE:1'`)

	rep, err := s.Erase(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// --- identity scrubbed ---
	if got := oneStr(t, db, `SELECT email FROM affiliates WHERE id = 1`); got != "erased+1@erased.invalid" {
		t.Errorf("affiliate email = %q", got)
	}
	for _, q := range []string{
		`SELECT COALESCE(name,'') FROM affiliates WHERE id = 1`,
		`SELECT COALESCE(postback_url,'') FROM affiliates WHERE id = 1`,
		`SELECT COALESCE(postback_secret,'') FROM affiliates WHERE id = 1`,
	} {
		if got := oneStr(t, db, q); got != "" {
			t.Errorf("identity field not blank: %q", got)
		}
	}
	if got := oneStr(t, db, `SELECT status FROM affiliates WHERE id = 1`); got != "removed" {
		t.Errorf("affiliate status = %q want removed", got)
	}

	// --- application, access, sessions ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM affiliate_applications WHERE email = 'subject@x.test'`); n != 0 {
		t.Errorf("application row survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM affiliate_sessions WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("sessions survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM portal_login_grants WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("grants survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM api_keys WHERE scope = 'affiliate:1' AND revoked_at IS NULL`); n != 0 {
		t.Errorf("api key still live: %d", n)
	}

	// --- clicks: rows kept, identity scrubbed ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM clicks WHERE affiliate_id = 1`); n != 1 {
		t.Errorf("click rows = %d (aggregates depend on them)", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM clicks WHERE affiliate_id = 1 AND ip_hash IS NOT NULL`); n != 0 {
		t.Errorf("click ip_hash survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM clicks WHERE affiliate_id = 1 AND (ua_hash IS NOT NULL OR referer != '' OR extra_json != '{}')`); n != 0 {
		t.Errorf("click identity fields survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM uniq_daily WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("uniq_daily survived: %d", n)
	}

	// --- attributions / customers severed ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM attributions WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("attributions survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM customers WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("customer bindings survived: %d", n)
	}

	// --- money preserved, PII columns blank ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commissions WHERE affiliate_id = 1 AND amount = 5000 AND status = 'pending'`); n != 1 {
		t.Errorf("commission money row damaged: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commissions WHERE affiliate_id = 1 AND
		(buyer_email != '' OR customer_ext_id != '' OR click_id IS NOT NULL OR order_id IS NOT NULL OR coupon_code != '')`); n != 0 {
		t.Errorf("commission PII survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM payouts WHERE id = 10 AND amount = 5000`); n != 1 {
		t.Errorf("payout row damaged: %d", n)
	}
	if got := oneStr(t, db, `SELECT COALESCE(sum0,'') FROM (SELECT '' AS sum0)`); got != "" {
		t.Errorf("sentinel")
	}
	// The books still balance, and payable:1 is unchanged by erasure.
	if got := oneInt(t, db, `SELECT COALESCE(SUM(amount),0) FROM ledger_entries`); got != sumZero {
		t.Errorf("ledger sum drifted: %d -> %d", sumZero, got)
	}
	if got := oneInt(t, db, `SELECT COALESCE(SUM(amount),0) FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id WHERE la.code = 'AFFILIATE_PAYABLE:1'`); got != payable1 {
		t.Errorf("payable:1 drifted: %d -> %d", payable1, got)
	}

	// --- enforcement history kept, text scrubbed ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM commission_holds WHERE commission_id = 1 AND reason = '[erased]'`); n != 1 {
		t.Errorf("hold not scrubbed: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM fraud_verdicts WHERE subject IN ('1','affiliate:1') AND reason != '[erased]'`); n != 0 {
		t.Errorf("subject fraud verdicts not scrubbed: %d", n)
	}
	// coupon_hijack verdicts keyed by the subject's coupon code must be
	// scrubbed even though their subject matches no id form.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM fraud_verdicts WHERE subject = 'savesubj' AND reason != '[erased]'`); n != 0 {
		t.Errorf("coupon-subject verdict not scrubbed: %d", n)
	}
	// cross-affiliate verdicts quoting the subject's email (raw or
	// alias forms) must be scrubbed wherever they sit.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM fraud_verdicts WHERE reason LIKE '%subject@x.test%' OR reason LIKE '%subject+x@x.test%'`); n != 0 {
		t.Errorf("cross-affiliate email residue in verdicts: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM fraud_verdicts WHERE subject IN ('ip:abcd','2') AND reason != '[erased]'`); n != 2 {
		t.Errorf("unrelated verdicts scrubbed (blast radius): %d", n)
	}

	// --- link identity dead ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM affiliate_codes WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("codes survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM coupons WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("coupons survived: %d", n)
	}

	// --- gamification + tax docs ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM xp_events WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("xp_events survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM tax_documents WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("tax_documents survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM tax_document_files WHERE affiliate_id = 1`); n != 0 {
		t.Errorf("tax_document_files survived: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM tax_document_files WHERE affiliate_id = 2`); n != 1 {
		t.Errorf("control tax_document_files row: %d want 1", n)
	}

	// --- import undo email blanked, batch intact ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM import_undos WHERE batch_id = 7`); n != 1 {
		t.Fatalf("import undo row deleted: %d", n)
	}
	undo := oneStr(t, db, `SELECT undo_json FROM import_undos WHERE batch_id = 7`)
	if containsAny(undo, "subject@x.test") {
		t.Errorf("undo json still carries subject email: %s", undo)
	}
	if containsAny(undo, "\"code\":\"subject\"") {
		t.Errorf("undo json still carries subject code: %s", undo)
	}
	if !containsAny(undo, "control@x.test") {
		t.Errorf("undo json lost the control entry (rollback broken): %s", undo)
	}

	// --- the global residue scan: no PII string anywhere it was seeded ---
	for table, col := range map[string]string{
		"affiliates": "email", "affiliate_applications": "email",
		"commissions": "buyer_email", "fraud_verdicts": "reason",
		"commission_holds": "reason", "tax_documents": "note",
	} {
		var n int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s LIKE '%%subject@x.test%%' OR %s LIKE '%%Subject One%%'`, table, col, col)
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("residue scan %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("PII residue in %s.%s: %d rows", table, col, n)
		}
	}

	// --- audit trail ---
	if n := oneInt(t, db, `SELECT COUNT(*) FROM audit_log WHERE table_name = 'affiliates' AND row_id = 1 AND action = 'erasure'`); n != 1 {
		t.Errorf("erasure audit row missing: %d", n)
	}
	if n := oneInt(t, db, `SELECT COUNT(*) FROM audit_log WHERE table_name = 'affiliates' AND row_id = 1 AND detail LIKE '%active->removed%'`); n != 1 {
		t.Errorf("status-transition audit row missing: %d", n)
	}

	// --- control affiliate: completely untouched ---
	for _, chk := range []struct {
		q    string
		want int64
		msg  string
	}{
		{`SELECT COUNT(*) FROM affiliates WHERE id = 2 AND email = 'control@x.test' AND status = 'active'`, 1, "affiliate row"},
		{`SELECT COUNT(*) FROM clicks WHERE affiliate_id = 2 AND ip_hash IS NOT NULL`, 1, "click hash"},
		{`SELECT COUNT(*) FROM attributions WHERE affiliate_id = 2`, 1, "attribution"},
		{`SELECT COUNT(*) FROM customers WHERE affiliate_id = 2`, 1, "customer binding"},
		{`SELECT COUNT(*) FROM commissions WHERE affiliate_id = 2 AND buyer_email = 'buyer2@shop.test'`, 1, "commission PII"},
		{`SELECT COUNT(*) FROM affiliate_codes WHERE affiliate_id = 2`, 1, "code"},
		{`SELECT COUNT(*) FROM coupons WHERE affiliate_id = 2`, 1, "coupon"},
		{`SELECT COUNT(*) FROM xp_totals WHERE affiliate_id = 2`, 1, "xp"},
		{`SELECT COUNT(*) FROM tax_documents WHERE affiliate_id = 2`, 1, "tax doc"},
		{`SELECT COUNT(*) FROM api_keys WHERE scope = 'affiliate:2' AND revoked_at IS NULL`, 1, "api key"},
		{`SELECT COUNT(*) FROM affiliate_sessions WHERE affiliate_id = 2`, 1, "session"},
	} {
		if got := oneInt(t, db, chk.q); got != chk.want {
			t.Errorf("control %s: got %d want %d", chk.msg, got, chk.want)
		}
	}

	_ = rep
}

// TestPrivacy_EraseIdempotent: the second erase touches nothing.
func TestPrivacy_EraseIdempotent(t *testing.T) {
	db := seedWorld(t)
	s := privacy.NewStore(db)
	ctx := context.Background()

	if _, err := s.Erase(ctx, 1); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Erase(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for table, n := range rep.Scrubbed {
		if n != 0 {
			t.Errorf("second erase scrubbed %s: %d rows", table, n)
		}
	}
	for table, n := range rep.Deleted {
		if table == "affiliate_applications" {
			continue // keyed by email; first run removed it
		}
		if n != 0 {
			t.Errorf("second erase deleted from %s: %d rows", table, n)
		}
	}
	// And only ONE erasure audit record total.
	if n := oneInt(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'erasure' AND row_id = 1`); n != 1 {
		t.Errorf("erasure audit rows = %d want 1", n)
	}
}

// TestPrivacy_EraseUnknownAffiliate: explicit error, no mutation.
func TestPrivacy_EraseUnknownAffiliate(t *testing.T) {
	db := seedWorld(t)
	s := privacy.NewStore(db)
	if _, err := s.Erase(context.Background(), 999); err != privacy.ErrUnknownAffiliate {
		t.Fatalf("err = %v", err)
	}
}

// TestPrivacy_EraseKeepsLedgerMath drills the invariant that matters most:
// after erasure the ledger still sums to zero per journal (the property
// the whole platform is built on), on both engines.
func TestPrivacy_EraseKeepsLedgerMath(t *testing.T) {
	db := seedWorld(t)
	s := privacy.NewStore(db)
	ctx := context.Background()

	var unbalanced int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT journal_id FROM ledger_entries GROUP BY journal_id
		HAVING SUM(amount) != 0 OR COUNT(*) < 2)`).Scan(&unbalanced); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 {
		t.Fatalf("pre-erase unbalanced journals: %d", unbalanced)
	}

	if _, err := s.Erase(ctx, 1); err != nil {
		t.Fatal(err)
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT journal_id FROM ledger_entries GROUP BY journal_id
		HAVING SUM(amount) != 0 OR COUNT(*) < 2)`).Scan(&unbalanced); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 {
		t.Fatalf("post-erase unbalanced journals: %d", unbalanced)
	}
	// No ledger row was touched at all (append-only, enforced by triggers,
	// but assert the count too — belt and braces).
	if n := oneInt(t, db, `SELECT COUNT(*) FROM ledger_entries`); n != 4 {
		t.Errorf("ledger entries = %d want 4", n)
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if s != "" && n != "" && len(s) >= len(n) && indexOf(s, n) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
