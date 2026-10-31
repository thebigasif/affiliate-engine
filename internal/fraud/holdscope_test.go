package fraud_test

// Hold-scope, fail-closed and threshold pins for the fraud engine
// (review of 6e1541e): verdicts must never blanket-hold an affiliate's
// balance, click-share rules need real conversions, one broken rule must
// not disable the rest, and DB errors must not be swallowed.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

var seedSeq atomic.Int64

// seedCommission inserts one commission for aff and returns its id.
func seedCommission(t *testing.T, h *db.Handles, aff int64, status, clickID string) int64 {
	t.Helper()
	key := fmt.Sprintf("hs-%d-%d", aff, seedSeq.Add(1))
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at)
		VALUES (?, 'conversion', CURRENT_TIMESTAMP)`, key); err != nil {
		t.Fatal(err)
	}
	var click any
	if clickID != "" {
		click = clickID
	}
	var id int64
	if err := h.DB.QueryRow(`INSERT INTO commissions
		(affiliate_id, journal_id, amount, rate_snapshot, status, created_at, click_id)
		VALUES (?, (SELECT id FROM journal_entries WHERE idem_key = ?), 100, '{}', ?, ?, ?)
		RETURNING id`,
		aff, key, status, time.Now().UTC().Format(time.RFC3339), click).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func openHolds(t *testing.T, h *db.Handles) map[int64]bool {
	t.Helper()
	rows, err := h.DB.Query(`SELECT commission_id FROM commission_holds WHERE resolved_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

// HS1: a verdict holds ONLY the evaluated commission — the affiliate's
// other open commissions keep flowing (no blanket hold).
func TestFraud_HoldTargetsEvaluatedCommissionOnly(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	older := seedCommission(t, h, 1, "approved", "")
	other := seedCommission(t, h, 1, "pending", "")
	target := seedCommission(t, h, 1, "pending", "")

	if _, err := e.Evaluate(context.Background(), fraud.Query{
		CommissionID: target, AffiliateID: 1, Email: "owner@gmail.com", Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	held := openHolds(t, h)
	if !held[target] {
		t.Fatal("evaluated commission not held")
	}
	if held[older] || held[other] {
		t.Fatalf("blanket hold: unrelated commissions held %v", held)
	}
}

// HS2: a sweep (no CommissionID) records verdicts but holds nothing that
// is not click-linked.
func TestFraud_SweepDoesNotBlanketHold(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	seedCommission(t, h, 1, "approved", "")
	vs, err := e.Evaluate(context.Background(), fraud.Query{
		AffiliateID: 1, Email: "owner@gmail.com", Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 {
		t.Fatal("sweep must still record the verdict")
	}
	if n := len(openHolds(t, h)); n != 0 {
		t.Fatalf("sweep blanket-held %d commissions", n)
	}
}

// HS3: click-based verdicts hold the commissions credited from the
// flagged clicks, and nothing else.
func TestFraud_ClickLinkedHold(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	ip := []byte{7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7}
	now := time.Now().UTC()
	for i := 0; i < 55; i++ {
		if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
			VALUES (?, ?, ?, ?, 1, 1, ?)`,
			fmt.Sprintf("burst%03d", i), now.Add(-10*time.Minute).Format(time.RFC3339Nano),
			now.Format("20060102"), now.Format("200601"), ip); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
		VALUES ('clean', ?, ?, ?, 1, 1)`,
		now.Format(time.RFC3339Nano), now.Format("20060102"), now.Format("200601")); err != nil {
		t.Fatal(err)
	}
	fromBurst := seedCommission(t, h, 1, "pending", "burst007")
	fromClean := seedCommission(t, h, 1, "pending", "clean")

	if _, err := e.Evaluate(context.Background(), fraud.Query{
		AffiliateID: 1, IPHash: ip, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	held := openHolds(t, h)
	if !held[fromBurst] {
		t.Fatal("commission credited from a flagged click not held")
	}
	if held[fromClean] {
		t.Fatal("commission from an unflagged click held")
	}
}

// HS4: a released commission is not silently re-held by a later verdict.
func TestFraud_ReleasedHoldNotReopened(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	cid := seedCommission(t, h, 1, "pending", "")
	if _, err := h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason, resolved_at, resolution)
		VALUES (?, 'earlier review', CURRENT_TIMESTAMP, 'released')`, cid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Evaluate(context.Background(), fraud.Query{
		CommissionID: cid, AffiliateID: 1, Email: "owner@gmail.com", Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if openHolds(t, h)[cid] {
		t.Fatal("released commission re-held")
	}
}

// HS5: duplicate_account reports ActionFreeze, but anyone can sign up
// with a victim's alias — the engine applies it as a hold on the
// evaluated commission and never freezes the account.
func TestFraud_DuplicateAccountNeverFreezes(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'owner+sock@gmail.com')`); err != nil {
		t.Fatal(err)
	}
	cid := seedCommission(t, h, 1, "pending", "")
	vs, err := e.Evaluate(context.Background(), fraud.Query{CommissionID: cid, AffiliateID: 1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vs {
		if v.Rule == "duplicate_account" {
			found = true
		}
	}
	if !found {
		t.Fatal("duplicate_account did not fire")
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM affiliates WHERE id = 1`).Scan(&status)
	if status != "active" {
		t.Fatalf("duplicate_account froze the account: %s", status)
	}
	if !openHolds(t, h)[cid] {
		t.Fatal("duplicate_account verdict did not hold the evaluated commission")
	}
}

type failingRule struct{}

func (failingRule) Name() string { return "broken_rule" }

func (failingRule) Evaluate(context.Context, fraud.Query) ([]fraud.Verdict, error) {
	return nil, errors.New("boom")
}

// FC1: one failing rule no longer skips the rest of the ruleset or the
// hold: later rules run, their verdicts apply, the error is returned.
func TestFraud_RuleErrorDoesNotFailOpen(t *testing.T) {
	h := newDB(t)
	cid := seedCommission(t, h, 1, "pending", "")
	rules := append([]fraud.Rule{failingRule{}}, fraud.NewEngine(h.DB).Rules()...)
	e := fraud.NewEngineWithRules(h.DB, rules...)
	vs, err := e.Evaluate(context.Background(), fraud.Query{
		CommissionID: cid, AffiliateID: 1, Email: "owner@gmail.com", Now: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "broken_rule") {
		t.Fatalf("rule error not surfaced: %v", err)
	}
	found := false
	for _, v := range vs {
		if v.Rule == "self_referral" {
			found = true
		}
	}
	if !found {
		t.Fatal("rules after the failing one were skipped")
	}
	if !openHolds(t, h)[cid] {
		t.Fatal("hold not applied when an unrelated rule failed")
	}
}

// FC2: DB errors surface from the sample-gated rules instead of reading
// as "not enough data" (the err==ErrNoRows||total<min shortcut).
func TestFraud_RuleDBErrorsSurface(t *testing.T) {
	closed, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	q := fraud.Query{AffiliateID: 1, Now: time.Now()}
	for _, r := range []fraud.Rule{
		fraud.ConversionTimeAnomaly{DB: closed},
		fraud.ASNReputation{DB: closed},
		fraud.BotClassGating{DB: closed},
		fraud.IPFallbackAnomaly{DB: closed},
	} {
		if _, err := r.Evaluate(context.Background(), q); err == nil {
			t.Errorf("%s swallowed a DB error", r.Name())
		}
	}
}

// CR1: click-share rules need >= 3 attributed conversions in the window
// before they enforce — junk clicks alone cannot trip them.
func TestFraud_ClickShareRulesNeedConversions(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := now.Format("20060102")
	if _, err := h.DB.Exec(`INSERT INTO dc_click_daily (day, affiliate_id, dc_clicks, total_clicks)
		VALUES (?, 1, 30, 30)`, day); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, bot_class)
			VALUES (?, ?, ?, ?, 1, 1, 1)`,
			fmt.Sprintf("sb%02d", i), now.Add(-time.Hour).Format(time.RFC3339Nano), day, now.Format("200601")); err != nil {
			t.Fatal(err)
		}
	}
	q := fraud.Query{AffiliateID: 1, Now: now}
	rules := []fraud.Rule{fraud.ASNReputation{DB: h.DB}, fraud.BotClassGating{DB: h.DB}}
	for _, r := range rules {
		vs, err := r.Evaluate(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) != 0 {
			t.Fatalf("%s enforced on clicks alone (no conversions): %+v", r.Name(), vs)
		}
	}
	for i := 0; i < 3; i++ {
		seedCommission(t, h, 1, "pending", "")
	}
	for _, r := range rules {
		vs, err := r.Evaluate(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) != 1 || vs[0].Action != fraud.ActionHold {
			t.Fatalf("%s silent with 3 conversions: %+v", r.Name(), vs)
		}
	}
}

// CH1: coupon_hijack reads bindings in the evaluated commission's program
// (was hard-coded program 1), trips at the new default of 3 overrides,
// freezes the COUPON OWNER, and the threshold is configurable.
func TestFraud_CouponHijackProgramAndThreshold(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO affiliates (id, email) VALUES (2, 'leaker@t.dev'), (3, 'victim@t.dev')`)
	mustExec(`INSERT INTO coupons (code, affiliate_id, campaign_id) VALUES ('LEAK', 2, 7)`)
	for i := 0; i < 3; i++ {
		cus := fmt.Sprintf("cus_%d", i)
		mustExec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id) VALUES (7, ?, 3)`, cus)
		key := fmt.Sprintf("ch-%d", i)
		mustExec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', CURRENT_TIMESTAMP)`, key)
		mustExec(`INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, rate_snapshot, status,
			created_at, attribution_method, coupon_code, customer_ext_id)
			VALUES (2, 7, (SELECT id FROM journal_entries WHERE idem_key = ?), 100, '{}', 'pending', ?, 'coupon', 'LEAK', ?)`,
			key, now.Format(time.RFC3339), cus)
	}

	// Program 1 (the old hard-code) sees no bindings: silent.
	e := fraud.NewEngine(h.DB)
	vs, err := e.Evaluate(ctx, fraud.Query{AffiliateID: 2, ProgramID: 1, CouponCode: "LEAK", Now: now, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "coupon_hijack" {
			t.Fatal("coupon_hijack matched bindings from the wrong program")
		}
	}

	// A raised threshold keeps it silent at 3 overrides.
	e.SetCouponHijackThreshold(4)
	vs, err = e.Evaluate(ctx, fraud.Query{AffiliateID: 2, ProgramID: 7, CouponCode: "LEAK", Now: now, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "coupon_hijack" {
			t.Fatal("configured threshold ignored")
		}
	}

	// Default threshold (3) in the right program: fires and freezes the
	// coupon owner (2), never the customers' bound affiliate (3).
	e.SetCouponHijackThreshold(0)
	vs, err = e.Evaluate(ctx, fraud.Query{AffiliateID: 2, ProgramID: 7, CouponCode: "LEAK", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vs {
		if v.Rule == "coupon_hijack" && v.Action == fraud.ActionFreeze {
			found = true
		}
	}
	if !found {
		t.Fatalf("coupon_hijack silent at the default threshold: %+v", vs)
	}
	var owner, victim string
	h.DB.QueryRow(`SELECT status FROM affiliates WHERE id = 2`).Scan(&owner)
	h.DB.QueryRow(`SELECT status FROM affiliates WHERE id = 3`).Scan(&victim)
	if owner != "frozen" || victim != "active" {
		t.Fatalf("freeze landed wrong: owner=%s victim=%s", owner, victim)
	}
}

// IA1: a commission credited to a frozen affiliate (e.g. an event naming
// the affiliate, resolved before the freeze) is held; active affiliates
// are untouched.
func TestFraud_InactiveAffiliateHeld(t *testing.T) {
	h := newDB(t)
	e := fraud.NewEngine(h.DB)
	active := seedCommission(t, h, 1, "pending", "")
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email, status) VALUES (2, 'frozen@t.dev', 'frozen')`); err != nil {
		t.Fatal(err)
	}
	frozen := seedCommission(t, h, 2, "pending", "")
	for _, q := range []fraud.Query{
		{CommissionID: active, AffiliateID: 1, Now: time.Now()},
		{CommissionID: frozen, AffiliateID: 2, Now: time.Now()},
	} {
		if _, err := e.Evaluate(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	held := openHolds(t, h)
	if !held[frozen] {
		t.Fatal("commission for a frozen affiliate not held")
	}
	if held[active] {
		t.Fatal("commission for an active affiliate held")
	}
}
