package importer_test

// The "switch offer": review before import, one batch per import, a
// one-command undo that refuses once money moved, idempotent re-runs, and
// the side-by-side compare. These legs run on dbtest (SQLite, and PG when
// AE_TEST_ENGINES includes it).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/importer"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

const switchAffiliates = "id,email,name,referral_id,status,coupon\n" +
	"a1,jane@shop.io,Jane,janeslink,active,JANE20\n" +
	"a2,bob@gmail.com,Bob,bobby,active,\n" +
	"a3,b.o.b+2@gmail.com,Bob Two,bob2,active,\n" +
	"a4,banned@shop.io,Bad,badguy,blocked,\n"

const switchReferrals = "id,affiliate_id,email,stripe_customer_id,status,created_at\n" +
	"r1,a1,cust1@x.com,cus_1,conversion,2026-09-20T10:00:00Z\n" +
	"r2,a1,jane+me@shop.io,cus_2,conversion,2026-09-21T11:00:00Z\n" +
	"r3,a2,cust3@x.com,cus_3,conversion,2026-09-22T12:00:00Z\n" +
	"r4,a2,lead@x.com,,lead,2026-09-22T12:00:00Z\n" +
	"r5,zz,who@x.com,cus_9,conversion,2026-09-22T12:00:00Z\n" +
	"r6,a3,spam@x.com,cus_6,rejected,2026-09-22T12:00:00Z\n"

const switchCommissions = "id,affiliate_email,customer_email,amount,currency,state,created_at\n" +
	"c1,jane@shop.io,cust1@x.com,10.00,USD,paid,2026-09-20T10:00:00Z\n" +
	"c2,jane@shop.io,jane+me@shop.io,12.50,usd,pending,2026-09-21T11:00:00Z\n" +
	"c3,bob@gmail.com,cust3@x.com,5,EUR,due,2026-09-22T12:00:00Z\n" +
	"c4,banned@shop.io,x@y.com,7,XXQ,due,2026-09-22T12:00:00Z\n" +
	"c5,banned@shop.io,x@y.com,abc,USD,due,2026-09-22T12:00:00Z\n" +
	"c6,bob@gmail.com,cust3@x.com,3.00,USD,voided,2026-09-22T13:00:00Z\n"

func switchDataset(t *testing.T, affCSV, refCSV, comCSV string) importer.Dataset {
	t.Helper()
	var ds importer.Dataset
	affs, _, err := importer.Parse(importer.Rewardful, strings.NewReader(affCSV))
	if err != nil {
		t.Fatal(err)
	}
	ds.Affiliates = affs
	if refCSV != "" {
		refs, info, err := importer.ParseReferrals(importer.Rewardful, strings.NewReader(refCSV))
		if err != nil {
			t.Fatal(err)
		}
		ds.Referrals, ds.Files = refs, append(ds.Files, info)
	}
	if comCSV != "" {
		coms, info, err := importer.ParseCommissions(importer.Rewardful, strings.NewReader(comCSV))
		if err != nil {
			t.Fatal(err)
		}
		ds.Commissions, ds.Files = coms, append(ds.Files, info)
	}
	return ds
}

// fullSnapshot renders every row an import or undo may touch, plus the
// append-only money tables (which neither may ever change).
func fullSnapshot(t *testing.T, h *db.Handles) string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT 'aff', id, email, name, status FROM affiliates`,
		`SELECT 'code', affiliate_id, code, is_primary, source FROM affiliate_codes`,
		`SELECT 'coupon', affiliate_id, code, rate_type, active FROM coupons`,
		`SELECT 'cust', id, external_customer_id, affiliate_id, bound_method FROM customers`,
		`SELECT 'acct', id, code, kind, '' FROM ledger_accounts`,
		`SELECT 'le', id, journal_id, account_id, amount FROM ledger_entries`,
		`SELECT 'je', id, idem_key, source, '' FROM journal_entries`,
		`SELECT 'com', id, affiliate_id, amount, status FROM commissions`,
	} {
		rows, err := h.DB.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, b, c, d, e any
			if err := rows.Scan(&a, &b, &c, &d, &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s|%v|%s|%v|%v", a, b, c, d, e))
		}
		rows.Close()
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func count(t *testing.T, h *db.Handles, q string, args ...any) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// SW1: the dry run writes nothing and reports counts, conflicts and the
// fraud found in the rival's history, rule by rule.
func TestSwitch_DryRunReview(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	before := fullSnapshot(t, h)
	ds := switchDataset(t, switchAffiliates, switchReferrals, switchCommissions)
	rep, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful, ds, true)
	if err != nil {
		t.Fatal(err)
	}
	if after := fullSnapshot(t, h); after != before {
		t.Fatalf("dry run wrote rows:\n%s\n->\n%s", before, after)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM import_batches`); n != 0 {
		t.Fatalf("dry run recorded %d batches", n)
	}
	if rep.AffiliatesCreated != 4 || rep.Codes != 4 || rep.Coupons != 1 || rep.Links != 4 {
		t.Fatalf("counts: %+v", rep)
	}
	// cus_1, cus_2, cus_3 bind; r4 has no customer id, r5 an unknown
	// affiliate, r6 was rejected by the rival.
	if rep.Referrals != 6 || rep.Bindings != 3 {
		t.Fatalf("referrals=%d bindings=%d, want 6/3", rep.Referrals, rep.Bindings)
	}
	if rep.Commissions != 5 || rep.CommissionTotals["USD"] != 2250 || rep.CommissionTotals["EUR"] != 500 {
		t.Fatalf("commissions=%d totals=%v", rep.Commissions, rep.CommissionTotals)
	}
	for kind, n := range map[string]int{"bad_amount": 1, "unknown_currency": 1, "no_customer_id": 1, "unknown_affiliate": 1} {
		if rep.Conflicts[kind] == nil || rep.Conflicts[kind].Count != n {
			t.Fatalf("conflict %s: %+v (all: %v)", kind, rep.Conflicts[kind], rep.Conflicts.Kinds())
		}
	}

	// Every live rule is accounted for, exactly once: ran, or a reason.
	status := map[string]importer.RuleStatus{}
	for _, s := range rep.Fraud.Rules {
		if _, dup := status[s.Rule]; dup {
			t.Fatalf("rule %s listed twice", s.Rule)
		}
		if !s.Ran && s.Why == "" {
			t.Fatalf("rule %s did not run and gave no reason", s.Rule)
		}
		status[s.Rule] = s
	}
	for _, r := range fraud.NewEngine(nil).Rules() {
		if _, ok := status[r.Name()]; !ok {
			t.Fatalf("live rule %s missing from the review", r.Name())
		}
	}
	for _, clickRule := range []string{"click_velocity", "asn_reputation", "ip_overlap", "bot_class_gating", "geo_mismatch", "ip_fallback_anomaly"} {
		if status[clickRule].Ran || !strings.Contains(status[clickRule].Why, "click-level") {
			t.Fatalf("%s: %+v", clickRule, status[clickRule])
		}
	}
	for _, r := range []string{"self_referral", "duplicate_account", "payment_fingerprint", "conversion_velocity", "inactive_affiliate", "conversion_time_anomaly"} {
		if !status[r].Ran {
			t.Fatalf("%s should run on this export: %+v", r, status[r])
		}
	}
	found := map[string]string{}
	for _, f := range rep.Fraud.Findings {
		found[f.Rule] = f.Subject
	}
	if found["self_referral"] != "jane@shop.io" || found["duplicate_account"] != "bob@gmail.com" || found["inactive_affiliate"] != "banned@shop.io" {
		t.Fatalf("findings: %+v", rep.Fraud.Findings)
	}
}

// SW2: rules the export cannot support say so, and date-only exports
// cannot run the time-of-day rules.
func TestSwitch_ReviewSaysWhatCouldNotRun(t *testing.T) {
	h := dbtest.Open(t)
	ds := switchDataset(t, switchAffiliates, "", "")
	rep, err := importer.MigrateDataset(context.Background(), h.DB, importer.Rewardful, ds, true)
	if err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, s := range rep.Fraud.Rules {
		if !s.Ran {
			why[s.Rule] = s.Why
		}
	}
	for rule, want := range map[string]string{
		"self_referral":       "customer email",
		"conversion_velocity": "commissions export",
		"coupon_hijack":       "coupon column",
		"inactive_affiliate":  "commissions export",
	} {
		if !strings.Contains(why[rule], want) {
			t.Fatalf("%s: reason %q, want mention of %q", rule, why[rule], want)
		}
	}

	dated := "id,affiliate_email,amount,currency,created_at\nc1,jane@shop.io,1,USD,2026-09-20\n"
	ds = switchDataset(t, switchAffiliates, "", dated)
	rep, err = importer.MigrateDataset(context.Background(), h.DB, importer.Rewardful, ds, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Fraud.Rules {
		if (s.Rule == "conversion_velocity" || s.Rule == "conversion_time_anomaly") && (s.Ran || !strings.Contains(s.Why, "time of day")) {
			t.Fatalf("%s on a date-only export: %+v", s.Rule, s)
		}
	}
}

// SW3: the time and coupon rules fire on the live thresholds.
func TestSwitch_ReviewThresholds(t *testing.T) {
	h := dbtest.Open(t)
	var com strings.Builder
	com.WriteString("id,affiliate_email,customer_id,coupon,amount,currency,created_at\n")
	base := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ { // 30 in one hour at 02:00: velocity + night share
		fmt.Fprintf(&com, "v%d,bob@gmail.com,cv%d,,1,USD,%s\n", i, i, base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339))
	}
	for i := 0; i < 40; i++ { // daytime baseline for the program
		fmt.Fprintf(&com, "d%d,banned@shop.io,cd%d,,1,USD,%s\n", i, i, base.Add(time.Duration(10+i)*time.Hour).Format(time.RFC3339))
	}
	// Customers first referred by bob, then credited to jane via her coupon.
	var ref strings.Builder
	ref.WriteString("id,affiliate_email,customer_id,created_at\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&ref, "h%d,bob@gmail.com,ch%d,2026-09-02T09:00:00Z\n", i, i)
		fmt.Fprintf(&com, "hc%d,jane@shop.io,ch%d,jane20,1,USD,2026-09-0%dT12:00:00Z\n", i, i, 3+i)
	}
	ds := switchDataset(t, switchAffiliates, ref.String(), com.String())
	rep, err := importer.MigrateDataset(context.Background(), h.DB, importer.Rewardful, ds, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range rep.Fraud.Findings {
		got[f.Rule] = f.Subject
	}
	if got["conversion_velocity"] != "bob@gmail.com" || got["conversion_time_anomaly"] != "bob@gmail.com" || got["coupon_hijack"] != "jane20" {
		t.Fatalf("findings: %+v", rep.Fraud.Findings)
	}
}

// SW4: import, re-run (nothing new, no batch), undo (exactly the pre-state,
// ledger untouched), undo again (no-op), re-import after undo (new batch).
func TestSwitch_UndoAndIdempotency(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	// Pre-state: jane already exists with her own code and a bound customer.
	for _, q := range []string{
		`INSERT INTO affiliates (id, email, name) VALUES (50, 'jane@shop.io', 'Jane (native)')`,
		`INSERT INTO affiliate_codes (affiliate_id, code, is_primary) VALUES (50, 'jane', TRUE)`,
		`INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method) VALUES (1, 'cus_1', 50, 'token')`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := fullSnapshot(t, h)
	ds := switchDataset(t, switchAffiliates, switchReferrals, switchCommissions)

	rep, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful, ds, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BatchID == 0 || rep.AffiliatesCreated != 3 || rep.Bindings != 2 {
		t.Fatalf("import: %+v", rep)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM customers WHERE bound_method = 'import:rewardful'`); n != 2 {
		t.Fatalf("imported bindings=%d want 2 (cus_1 was already bound)", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM journal_entries`); n != 0 {
		t.Fatalf("import posted %d journals: money must never be imported", n)
	}
	afterImport := fullSnapshot(t, h)

	// Re-run: nothing new, no batch, nothing changed.
	again, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful, ds, false)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NothingNew || again.BatchID != 0 || again.PreviousBatch != rep.BatchID || fullSnapshot(t, h) != afterImport {
		t.Fatalf("re-run: %+v", again)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM import_batches`); n != 1 {
		t.Fatalf("re-run recorded a batch: %d", n)
	}

	u, err := importer.Undo(ctx, h.DB, rep.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Affiliates != 3 || u.Bindings != 2 || u.Coupons != 1 || u.Codes != 4 || u.LedgerAccounts != 0 { // payables are created on first post, not with the affiliate
		t.Fatalf("undo report: %+v", u)
	}
	if after := fullSnapshot(t, h); after != before {
		t.Fatalf("undo did not restore the pre-import state\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM audit_log WHERE table_name = 'import_batches' AND action = 'undo'`); n != 1 {
		t.Fatalf("undo audit rows=%d", n)
	}

	u2, err := importer.Undo(ctx, h.DB, rep.BatchID)
	if err != nil || !u2.AlreadyUndone {
		t.Fatalf("second undo: %+v %v", u2, err)
	}
	if fullSnapshot(t, h) != before {
		t.Fatal("second undo changed data")
	}

	// The undone batch no longer counts as "already imported".
	re, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful, ds, false)
	if err != nil {
		t.Fatal(err)
	}
	if re.NothingNew || re.BatchID == 0 || re.BatchID == rep.BatchID || re.AffiliatesCreated != 3 {
		t.Fatalf("re-import after undo: %+v", re)
	}
	bs, err := importer.Batches(ctx, h.DB)
	if err != nil || len(bs) != 2 || bs[0].UndoneAt != "" || bs[1].UndoneAt == "" {
		t.Fatalf("batches: %+v %v", bs, err)
	}
}

// SW5: a changed export imports only its delta as a new batch; undoing
// the first batch while the second hangs codes off its affiliates is
// refused; undoing newest-first works.
func TestSwitch_DeltaBatchesUndoInOrder(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	before := fullSnapshot(t, h)
	first, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful,
		switchDataset(t, "email,referral_id\nann@x.io,ann\n", "", ""), false)
	if err != nil {
		t.Fatal(err)
	}
	// Tolt export of the same person with another code, plus a newcomer.
	second, err := importer.MigrateDataset(ctx, h.DB, importer.Tolt,
		switchDataset(t, "email,referral_id\nann@x.io,ann-tolt\nzed@x.io,zed\n", "", ""), false)
	if err != nil {
		t.Fatal(err)
	}
	if second.AffiliatesCreated != 1 || second.Codes != 2 {
		t.Fatalf("delta batch: %+v", second)
	}
	mid := fullSnapshot(t, h)
	_, err = importer.Undo(ctx, h.DB, first.BatchID)
	if !errors.Is(err, importer.ErrUndoRefused) || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("undo of the older batch: %v", err)
	}
	if fullSnapshot(t, h) != mid {
		t.Fatal("refused undo changed data")
	}
	if _, err := importer.Undo(ctx, h.DB, second.BatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Undo(ctx, h.DB, first.BatchID); err != nil {
		t.Fatal(err)
	}
	if fullSnapshot(t, h) != before {
		t.Fatal("both undos did not restore the empty state")
	}
}

type moneyEnv struct {
	h *db.Handles
	e *commissions.Engine
}

func newMoneyEnv(t *testing.T) *moneyEnv {
	t.Helper()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (1, NULL, 'purchase', 'percent', 1000)`); err != nil {
		t.Fatal(err)
	}
	return &moneyEnv{h: h, e: commissions.NewEngine(h.DB, ledger.NewPoster(h.Ledger))}
}

func (m *moneyEnv) buy(t *testing.T, id string, ev commissions.Event) {
	t.Helper()
	ev.Source, ev.ExternalID, ev.EventType = "stripe", id, "purchase"
	ev.AmountMinor, ev.Currency, ev.CampaignID, ev.OccurredAt = 10000, "USD", 1, time.Now().UTC()
	if _, err := m.e.ProcessPurchase(context.Background(), ev); err != nil {
		t.Fatalf("purchase %s: %v", id, err)
	}
}

// SW6: once money moved against what a batch created, undo refuses and
// changes nothing: a commission to an imported affiliate, a renewal
// credited through an imported customer binding (even to a pre-existing
// affiliate), a sale credited through an imported coupon.
func TestSwitch_UndoRefusesWhenMoneyMoved(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		pre  []string
		sale func(t *testing.T, m *moneyEnv)
		want string
	}{
		"commission to an imported affiliate": {
			sale: func(t *testing.T, m *moneyEnv) {
				var id int64
				_ = m.h.DB.QueryRow(`SELECT id FROM affiliates WHERE email = 'bob@gmail.com'`).Scan(&id)
				m.buy(t, "evt_aff", commissions.Event{AffiliateID: id})
			},
			want: "commissions since the import",
		},
		"renewal through an imported binding": {
			pre: []string{`INSERT INTO affiliates (id, email) VALUES (50, 'jane@shop.io')`},
			sale: func(t *testing.T, m *moneyEnv) {
				m.buy(t, "evt_renewal", commissions.Event{CustomerExtID: "cus_2"})
			},
			want: "imported customer binding",
		},
		"sale through an imported coupon": {
			pre: []string{`INSERT INTO affiliates (id, email) VALUES (50, 'jane@shop.io')`},
			sale: func(t *testing.T, m *moneyEnv) {
				m.buy(t, "evt_coupon", commissions.Event{CouponCode: "jane20"})
			},
			want: "imported coupon jane20",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMoneyEnv(t)
			for _, q := range tc.pre {
				if _, err := m.h.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := importer.MigrateDataset(ctx, m.h.DB, importer.Rewardful,
				switchDataset(t, switchAffiliates, switchReferrals, ""), false)
			if err != nil {
				t.Fatal(err)
			}
			tc.sale(t, m)
			frozen := fullSnapshot(t, m.h)
			_, err = importer.Undo(ctx, m.h.DB, rep.BatchID)
			if !errors.Is(err, importer.ErrUndoRefused) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "money moved") {
				t.Fatalf("undo after a sale: %v (want %q)", err, tc.want)
			}
			if fullSnapshot(t, m.h) != frozen {
				t.Fatal("refused undo changed data")
			}
			var undone any
			_ = m.h.DB.QueryRow(`SELECT undone_at FROM import_batch_state WHERE batch_id = ?`, rep.BatchID).Scan(&undone)
			if undone != nil {
				t.Fatalf("refused undo marked the batch undone: %v", undone)
			}
		})
	}
}

// SW7: a paid payout is money moved too (the commission check covers it,
// the payout check names it).
func TestSwitch_UndoRefusesAfterPayout(t *testing.T) {
	ctx := context.Background()
	m := newMoneyEnv(t)
	rep, err := importer.MigrateDataset(ctx, m.h.DB, importer.Rewardful,
		switchDataset(t, switchAffiliates, "", ""), false)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = m.h.DB.QueryRow(`SELECT id FROM affiliates WHERE email = 'jane@shop.io'`).Scan(&id)
	if _, err := m.h.DB.Exec(`INSERT INTO payouts (affiliate_id, amount, status) VALUES (?, 100, 'paid')`, id); err != nil {
		t.Fatal(err)
	}
	_, err = importer.Undo(ctx, m.h.DB, rep.BatchID)
	if !errors.Is(err, importer.ErrUndoRefused) || !strings.Contains(err.Error(), "payouts") {
		t.Fatalf("undo after a payout: %v", err)
	}
}

// SW8: clicks on an imported affiliate block the undo too (deleting the
// affiliate would orphan them); the error says so.
func TestSwitch_UndoRefusesWithTraffic(t *testing.T) {
	h := dbtest.Open(t)
	ctx := context.Background()
	rep, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful, switchDataset(t, switchAffiliates, "", ""), false)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = h.DB.QueryRow(`SELECT id FROM affiliates WHERE email = 'bob@gmail.com'`).Scan(&id)
	if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id) VALUES ('c1', ?, 20260929, 202609, ?)`,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	_, err = importer.Undo(ctx, h.DB, rep.BatchID)
	if !errors.Is(err, importer.ErrUndoRefused) || !strings.Contains(err.Error(), "1 clicks") {
		t.Fatalf("undo with clicks: %v", err)
	}
}

// SW8b: a referral code the batch attached to a PRE-EXISTING affiliate is
// in use (clicks, attributions through it since the import): undo refuses
// instead of deleting a live link. Traffic on the affiliate's own native
// code does not block it.
func TestSwitch_UndoRefusesWhenImportedCodeUsed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, code  string
		attribution bool
		refuse      bool
		want        string
	}{
		{"click on the imported code", "janeslink", false, true, "imported referral code janeslink (affiliate 50 (jane@shop.io)) has 1 clicks since the import"},
		{"attribution on the imported code", "janeslink", true, true, "has 1 attributions since the import"},
		{"click on the native code only", "jane", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := dbtest.Open(t)
			for _, q := range []string{
				`INSERT INTO affiliates (id, email, name) VALUES (50, 'jane@shop.io', 'Jane (native)')`,
				`INSERT INTO affiliate_codes (affiliate_id, code, is_primary) VALUES (50, 'jane', TRUE)`,
			} {
				if _, err := h.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := importer.MigrateDataset(ctx, h.DB, importer.Rewardful, switchDataset(t, switchAffiliates, "", ""), false)
			if err != nil {
				t.Fatal(err)
			}
			if n := count(t, h, `SELECT COUNT(*) FROM affiliate_codes WHERE affiliate_id = 50 AND code = 'janeslink'`); n != 1 {
				t.Fatalf("import did not attach janeslink to the existing affiliate (%d)", n)
			}
			now := time.Now().UTC().Add(2 * time.Second)
			if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, ref_code) VALUES ('c1', ?, 20260929, 202609, 50, ?)`,
				now.Format(time.RFC3339Nano), tc.code); err != nil {
				t.Fatal(err)
			}
			if tc.attribution {
				// The attribution alone must block: drop the click's own
				// timestamp below the cutoff so only the attribution counts.
				if _, err := h.DB.Exec(`UPDATE clicks SET ts = ? WHERE id = 'c1'`, "2000-01-01T00:00:00Z"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.DB.Exec(`INSERT INTO attributions (click_id, affiliate_id, method, created_at, expires_at) VALUES ('c1', 50, 'token', ?, ?)`,
					now.Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			}
			frozen := fullSnapshot(t, h)
			_, err = importer.Undo(ctx, h.DB, rep.BatchID)
			if !tc.refuse {
				if err != nil {
					t.Fatalf("undo with native-code traffic only: %v", err)
				}
				if n := count(t, h, `SELECT COUNT(*) FROM affiliate_codes WHERE code = 'janeslink'`); n != 0 {
					t.Fatal("undo left the imported code")
				}
				return
			}
			if !errors.Is(err, importer.ErrUndoRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("undo with imported code in use: %v (want %q)", err, tc.want)
			}
			if fullSnapshot(t, h) != frozen {
				t.Fatal("refused undo changed data")
			}
		})
	}
}

// SW9: side by side — their totals next to ours for the same days.
func TestSwitch_Compare(t *testing.T) {
	ctx := context.Background()
	m := newMoneyEnv(t)
	if _, err := importer.MigrateDataset(ctx, m.h.DB, importer.Rewardful,
		switchDataset(t, switchAffiliates, switchReferrals, ""), false); err != nil {
		t.Fatal(err)
	}
	var jane int64
	_ = m.h.DB.QueryRow(`SELECT id FROM affiliates WHERE email = 'jane@shop.io'`).Scan(&jane)
	day := time.Now().UTC()
	// Ours today: 2 clicks (plus a bot that does not count), one new
	// customer, one $10 commission. Imported bindings do not count.
	for i, excl := range []int{0, 0, 1} {
		if _, err := m.h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, exclude_stats) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("k%d", i), day.Format(time.RFC3339), day.Format("20060102"), day.Format("200601"), jane, excl); err != nil {
			t.Fatal(err)
		}
	}
	m.buy(t, "evt_today", commissions.Event{AffiliateID: jane, CustomerExtID: "cus_new"})

	d := day.Format("2006-01-02")
	refs, _, _ := importer.ParseReferrals(importer.Rewardful, strings.NewReader(
		"id,affiliate_id,stripe_customer_id,status,created_at\n"+
			"t1,a1,cus_new,conversion,"+d+"T08:00:00Z\n"+
			"t2,a1,cus_x,conversion,"+d+"T09:00:00Z\n"+
			"t3,a1,,lead,"+d+"T09:00:00Z\n"+
			"old,a1,cus_o,conversion,2020-01-01T09:00:00Z\n"))
	coms, _, _ := importer.ParseCommissions(importer.Rewardful, strings.NewReader(
		"id,affiliate_email,amount,currency,state,created_at\n"+
			"x1,jane@shop.io,10.00,USD,due,"+d+"T08:00:00Z\n"+
			"x2,jane@shop.io,4.00,USD,due,"+d+"T09:00:00Z\n"+
			"x3,jane@shop.io,4.00,USD,voided,"+d+"T09:00:00Z\n"))
	clk, _, _ := importer.ParseClicks(importer.Rewardful, strings.NewReader("date,visits\n"+d+",5\n2020-01-01,9\n"))
	rep, err := importer.Compare(ctx, m.h.DB, importer.Rewardful, importer.CompareInput{
		Referrals: refs, Commissions: coms, Clicks: clk,
		HasReferrals: true, HasCommissions: true, HasClicks: true, From: day, To: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rep.Rows {
		k := r.Metric + " " + r.Currency
		got[strings.TrimSpace(k)] = fmt.Sprintf("%d/%d/%d", *r.Theirs, r.Ours, *r.Diff)
	}
	want := map[string]string{
		"clicks":                "5/2/-3",
		"referrals":             "2/1/-1",
		"commissions":           "2/1/-1",
		"commission amount USD": "1400/1000/-400",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %s want %s (all %v)", k, got[k], v, got)
		}
	}
}

// SW10: rival amount/date/currency shapes.
func TestSwitch_ParseShapes(t *testing.T) {
	coms, info, err := importer.ParseCommissions(importer.FirstPromoter, strings.NewReader(
		"Commission ID,Promoter Email,Amount Cents,Currency,Status,Created At\n"+
			"1,a@x.io,1250,usd,approved,2026-09-01 10:00:00\n"+
			"2,a@x.io,300,JPY,approved,09/02/2026\n"))
	if err != nil || len(coms) != 2 || coms[0].Amount != 1250 || coms[0].Currency != "USD" || coms[1].Amount != 300 {
		t.Fatalf("cents shape: %+v %v", coms, err)
	}
	if coms[1].At.Format("2006-01-02") != "2026-09-02" || info.Issues["bad_date"] != nil {
		t.Fatalf("dates: %+v %+v", coms, info.Issues)
	}
	coms, _, err = importer.ParseCommissions(importer.Tolt, strings.NewReader(
		"id,affiliate_email,amount,currency\n1,a@x.io,\"$1,234.56\",USD\n2,a@x.io,\"12,5\",EUR\n3,a@x.io,500,JPY\n4,a@x.io,-3.10,USD\n"))
	if err != nil || coms[0].Amount != 123456 || coms[1].Amount != 1250 || coms[2].Amount != 500 || coms[3].Amount != -310 {
		t.Fatalf("decimal shapes: %+v %v", coms, err)
	}
	if _, _, err := importer.ParseCommissions(importer.Tolt, strings.NewReader("id,email\n1,a@x.io\n")); err == nil {
		t.Fatal("commissions without amount/affiliate columns accepted")
	}
	refs, _, err := importer.ParseReferrals(importer.Tapfiliate, strings.NewReader(
		"\ufeffID,Affiliate ID,Customer Email,Customer ID,State,Created\nr1,aff9,C@X.io,cus_1,converted,2026-09-01\n"))
	if err != nil || len(refs) != 1 || refs[0].AffiliateExtID != "aff9" || refs[0].CustomerEmail != "c@x.io" || refs[0].CustomerID != "cus_1" {
		t.Fatalf("referral shape: %+v %v", refs, err)
	}
}
