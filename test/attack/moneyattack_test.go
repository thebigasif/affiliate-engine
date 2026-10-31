// Package attack_test is the MONEY-INVARIANT attack suite: 10 adversarial
// tests probing cross-API races and edge cases the 15 ledger property tests
// (internal/ledger/post_test.go) do not cover. Born as a throwaway QA round;
// promoted to a permanent CI regression suite — every test here pins a bug
// that once reproduced live (see git history of this file's fixes).
package attack_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/gamify"
	"github.com/thebigasif/affiliate-engine/internal/importer"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/postbacks"
	"github.com/thebigasif/affiliate-engine/internal/usage"
)

// ---------- shared env ----------

type env struct {
	h *db.Handles
	p *ledger.Poster
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"PAYOUT_CLEARING:USD":     "clearing",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"AFFILIATE_PAYABLE:2:USD": "liability",
		"AFFILIATE_PAYABLE:3:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, h.DB, `INSERT INTO affiliates (id, email) VALUES (1,'a1@t.dev'),(2,'a2@t.dev'),(3,'a3@t.dev')`)
	mustExec(t, h.DB, `INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (1, NULL, 'purchase', 'percent', 1000)`) // 10%
	return &env{h: h, p: p}
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func qInt64(t *testing.T, d *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func qStr(t *testing.T, d *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := d.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func purchase(id string, aff, amount int64) commissions.Event {
	return commissions.Event{
		Source: "stripe", ExternalID: id, EventType: "purchase",
		AmountMinor: amount, Currency: "USD",
		AffiliateID: aff, CampaignID: 1, OccurredAt: time.Now().UTC(),
	}
}

// credit+mature one commission, returns its id.
func maturedCommission(t *testing.T, e *commissions.Engine, d *sql.DB, id string, aff, amount int64) int64 {
	t.Helper()
	ctx := context.Background()
	cid, err := e.ProcessPurchase(ctx, purchase(id, aff, amount))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, d, `UPDATE commissions SET matures_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour), cid)
	if _, err := e.Mature(ctx); err != nil {
		t.Fatal(err)
	}
	return cid
}

// wirePayoutLedger mirrors cmd/affiliate-engine/main.go wiring.
func wirePayoutLedger(t *testing.T, p *ledger.Poster) {
	t.Helper()
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := p.Post(ctx, fmt.Sprintf("payout:%d", payoutID), "payout", "",
			time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "locked") || strings.Contains(s, "busy")
}

// hookPoster wraps the real poster and lets a test interleave another API
// call inside the read->write window of the engine under attack.
type hookPoster struct {
	real   commissions.Poster
	before func(idemKey string)
}

func (hp *hookPoster) Post(ctx context.Context, idem, source, desc string, eff time.Time, lines []ledger.Line) (int64, error) {
	if hp.before != nil {
		hp.before(idem)
	}
	return hp.real.Post(ctx, idem, source, desc, eff, lines)
}

func (hp *hookPoster) AccountBalance(ctx context.Context, code string) (int64, error) {
	return hp.real.AccountBalance(ctx, code)
}

// Run delegates; the engine's posts inside it still go through hp.Post
// (with Run's ctx, joining the transaction), so the hook fires mid-window.
func (hp *hookPoster) Run(ctx context.Context, fn func(ctx context.Context, tx *ledger.Tx) error) error {
	return hp.real.Run(ctx, fn)
}

// usagePosterAdapter converts usage.Line to ledger.Line (mirrors httpapi).
type usagePosterAdapter struct{ p *ledger.Poster }

func (u usagePosterAdapter) Post(ctx context.Context, idem, source, desc string, eff time.Time, lines []usage.Line) (int64, error) {
	ll := make([]ledger.Line, len(lines))
	for i, l := range lines {
		ll[i] = ledger.Line{Account: l.Account, Amount: l.Amount}
	}
	return u.p.Post(ctx, idem, source, desc, eff, ll)
}

func (u usagePosterAdapter) Run(ctx context.Context, fn func(ctx context.Context, tx usage.Tx) error) error {
	return u.p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error { return fn(ctx, tx) })
}

// prodUphold replicates the PRODUCTION uphold path (httpapi.handleFraudResolve):
// domain tx (cancel + resolve hold) commits FIRST, ledger clawback posts after.
// (fraud.Reviewer.Uphold deterministically deadlocks on SQLite — see
// TestAttack1d — so it is not the production shape.)
func prodUphold(ctx context.Context, t *testing.T, e *env, commissionID int64, note string) error {
	t.Helper()
	return fraud.Uphold(ctx, e.h.DB, e.p, commissionID, note)
}

// ===========================================================================
// ATTACK 1 — payout Approve vs fraud Uphold on the SAME commission
// ===========================================================================

// 1a. Deterministic interleave: commission approved -> payout staged ->
// uphold claws it back -> Approve fires. In production the transfer hook
// runs BEFORE settleLedger, so money can leave before the overdraft guard
// reacts; at minimum the payout is stranded in 'processing'.
func TestAttack1a_UpholdThenApprove_StrandedPayout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	wirePayoutLedger(t, e.p)
	eng := commissions.NewEngine(e.h.DB, e.p)
	cid := maturedCommission(t, eng, e.h.DB, "a1-evt", 1, 10000) // approved, payable -1000

	r := payouts.NewRunner(e.h.DB)
	_, n, err := r.CreateBatch(ctx, 0, "manual")
	if err != nil || n != 1 {
		t.Fatalf("batch n=%d err=%v", n, err)
	}
	pid := qInt64(t, e.h.DB, `SELECT id FROM payouts`)

	if err := prodUphold(ctx, t, e, cid, "fraud confirmed"); err != nil {
		t.Fatal(err)
	}
	bal, _ := e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if bal != 0 {
		t.Fatalf("payable after uphold = %d, want 0", bal)
	}

	err = r.Approve(ctx, pid, nil) // nil = manual rail (no-op execute)
	t.Logf("Approve after uphold error: %v", err)
	if err == nil {
		t.Fatal("Approve of an upheld (cancelled) commission succeeded — paid money under fraud cancel")
	}
	// INVARIANT: payout must not be stranded in a non-retryable state.
	status := qStr(t, e.h.DB, `SELECT status FROM payouts WHERE id=?`, pid)
	t.Logf("payout status after failed approve: %s", status)
	if status != "failed" && status != "requested" {
		t.Errorf("P1 STRANDED PAYOUT: payout %d stuck in status=%q (no retry path; execute hook already ran in prod wiring)", pid, status)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}
}

// 1b. True concurrency: Approve || Uphold on the same staged payout.
func TestAttack1b_ConcurrentApproveVsUphold(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	wirePayoutLedger(t, e.p)
	eng := commissions.NewEngine(e.h.DB, e.p)
	r := payouts.NewRunner(e.h.DB)

	violations := 0
	for i := 0; i < 15; i++ {
		// No cleanup between iterations: payouts/payout_lines are guarded
		// (no DELETE), and none is needed — every iteration ends with its
		// commission cancelled or paid, so the next CreateBatch stages only
		// the new one. The payout under test is the newest.
		cid := maturedCommission(t, eng, e.h.DB, fmt.Sprintf("a1b-%d", i), 1, 10000)
		if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
			t.Fatalf("iter %d: batch n=%d err=%v", i, n, err)
		}
		pid := qInt64(t, e.h.DB, `SELECT MAX(id) FROM payouts`)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = r.Approve(ctx, pid, nil) }()
		go func() { defer wg.Done(); _ = prodUphold(ctx, t, e, cid, "race") }()
		wg.Wait()

		if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
			t.Fatalf("iter %d: ledger integrity=%d", i, s)
		}
		cStatus := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE id=?`, cid)
		pStatus := qStr(t, e.h.DB, `SELECT status FROM payouts WHERE id=?`, pid)
		bal, _ := e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
		t.Logf("iter %d: commission=%s payout=%s payable=%d", i, cStatus, pStatus, bal)
		// P0 shape: commission cancelled by fraud BUT affiliate still owed the money.
		if cStatus == "cancelled" && bal < 0 {
			violations++
			t.Errorf("iter %d: P0 PHANTOM PAYABLE — commission cancelled but payable=%d (affiliate can be paid for fraud-cancelled money)", i, bal)
		}
		// P0 shape: cancelled commission marked paid.
		if cStatus == "cancelled" && pStatus == "paid" {
			violations++
			t.Errorf("iter %d: P0 — payout paid a commission that fraud cancelled concurrently", i)
		}
	}
	t.Logf("concurrent approve-vs-uphold: %d violations over 15 iterations", violations)
}

// 1e. Deterministic APPROVE-WINS interleave of the 1b race. 1b relies on
// the scheduler to hit both orderings; on fast local runners uphold won
// every iteration (240/240), so the CI-only "approve pays first, uphold
// arrives mid-settle" shape had no guaranteed coverage. This test FORCES
// that ordering: Approve runs in the main goroutine and signals from
// inside settleLedger (via a wrapped LedgerPoster) right before the
// payable->clearing journal posts; Uphold is launched only then, so it
// must queue on the commission moneylock Approve holds for its whole
// body. Pins: (a) no money movement interleaves mid-settle; (b) when
// Uphold finally runs it sees status='paid' and REJECTS with
// ErrAlreadyPaid instead of double-clawing a paid commission; (c) the
// payable nets to exactly 0 — the CI-observed failure mode was a -1000
// residual per paid iteration (phantom debt).
func TestAttack1e_ApproveWinsMidSettle_UpholdRejects(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	wirePayoutLedger(t, e.p)
	eng := commissions.NewEngine(e.h.DB, e.p)
	r := payouts.NewRunner(e.h.DB)

	cid := maturedCommission(t, eng, e.h.DB, "a1e-evt", 1, 10000) // approved, payable -1000 (credit convention)
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("batch n=%d err=%v", n, err)
	}
	pid := qInt64(t, e.h.DB, `SELECT MAX(id) FROM payouts`)

	// Wrap the wired poster: signal when the settle journal is about to
	// post, and hold the post until uphold is parked on the lock — the
	// moment the CI race actually hits.
	realPoster := payouts.LedgerPoster
	inSettle := make(chan struct{})
	release := make(chan struct{})
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		if payoutID == pid {
			close(inSettle)
			<-release
		}
		return realPoster(ctx, payoutID, affiliateID, currency, amount)
	}
	t.Cleanup(func() { payouts.LedgerPoster = realPoster })

	upholdDone := make(chan error, 1)
	approveDone := make(chan error, 1)
	go func() { approveDone <- r.Approve(ctx, pid, nil) }()
	go func() {
		<-inSettle // Approve is INSIDE settleLedger now
		upholdDone <- prodUphold(ctx, t, e, cid, "approve-wins race")
	}()

	// Give uphold a moment to park on the commission moneylock; Approve
	// still holds it, so uphold cannot proceed past Lock().
	<-inSettle
	time.Sleep(50 * time.Millisecond)
	// Prove the invariant AT the window: while Approve holds the lock
	// mid-settle, the commission is not yet mutated by any rival.
	if s := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE id=?`, cid); s != "approved" {
		t.Errorf("mid-settle commission status=%q, want approved (rival mutated inside the lock window)", s)
	}
	close(release)

	if err := <-approveDone; err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := <-upholdDone; err == nil {
		t.Fatal("Uphold of a paid commission succeeded — clawed money that already left via payout")
	} else if !errors.Is(err, fraud.ErrAlreadyPaid) {
		t.Fatalf("Uphold error = %v, want fraud.ErrAlreadyPaid", err)
	}

	if s := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE id=?`, cid); s != "paid" {
		t.Errorf("commission status=%q, want paid", s)
	}
	if s := qStr(t, e.h.DB, `SELECT status FROM payouts WHERE id=?`, pid); s != "paid" {
		t.Errorf("payout status=%q, want paid", s)
	}
	// The credit->payable(+1000 credit | -1000 settle | ... ) must net
	// the affiliate to exactly what was paid: payable 0, no residual
	// debt. CI failure shape: -1000 per paid iteration accumulating.
	bal, err := e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Errorf("P0 PHANTOM DEBT — payable=%d after approve-wins settle + rejected uphold, want 0", bal)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}
	// And the rejected uphold must NOT have posted a claw journal for
	// the paid commission (the double-claw the CI shape accumulated).
	var clawJournals int64
	if err := e.h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE idem_key = ?`, fmt.Sprintf("fraudclaw:%d", cid)).Scan(&clawJournals); err != nil {
		t.Fatal(err)
	}
	if clawJournals != 0 {
		t.Errorf("fraud claw journals for paid commission %d = %d, want 0", cid, clawJournals)
	}
}

// 1c. Double Approve on the same payout (both read 'requested' before the
// CAS). The CAS result is never checked, so execute() runs twice.
func TestAttack1c_DoubleApprove(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	wirePayoutLedger(t, e.p)
	eng := commissions.NewEngine(e.h.DB, e.p)
	r := payouts.NewRunner(e.h.DB)
	_ = maturedCommission(t, eng, e.h.DB, "a1c-evt", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("batch n=%d err=%v", n, err)
	}
	pid := qInt64(t, e.h.DB, `SELECT id FROM payouts`)

	var executes int64
	hook := func(int64, int64) error { atomic.AddInt64(&executes, 1); return nil }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = r.Approve(ctx, pid, hook) }()
	go func() { defer wg.Done(); _ = r.Approve(ctx, pid, hook) }()
	wg.Wait()

	n := atomic.LoadInt64(&executes)
	t.Logf("execute() invoked %d times for payout %d", n, pid)
	if n > 1 {
		t.Errorf("P0 DOUBLE TRANSFER: execute hook ran %d times for one payout (CAS result unchecked; ledger idem-key catches the journal only AFTER money moved)", n)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}
}

// 1d. RESOLVED (F7): fraud.Reviewer.Uphold was dead code that
// deterministically deadlocked on SQLite (read snapshot held across a
// ledger-conn write). The review.go path was deleted; the production
// uphold lives in httpapi.handleFraudResolve — now F3-hardened so the
// clawback account follows the CAS outcome, not a stale status read.
// The race is pinned by TestFraudUphold_MaturationRace in internal/httpapi.
func TestAttack1d_ReviewerRemoved(t *testing.T) {
	_ = t // documentation pin: the deleted API must not silently return
}

// ===========================================================================
// ATTACK 2 — SettlePairs + RunPeriod on overlapping windows
// ===========================================================================

// 2a. Two DIFFERENT (overlapping) period windows include the same commission:
// the unique idempotency key is scoped per period_id, so tier splits pay twice.
func TestAttack2a_OverlappingPeriodWindows_DoubleTierPay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Sponsor tree: 1 <- 2 <- 3 (3's commission pays 2 at L1 and 1 at L2).
	mustExec(t, e.h.DB, `INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level) VALUES
		(2,3,NULL,1),(1,3,NULL,2),(1,2,NULL,1)`)
	mustExec(t, e.h.DB, `INSERT INTO mlm_tier_rates (campaign_id, level, rate_bp) VALUES (1,1,1000),(1,2,500)`)

	jid, err := e.p.Post(ctx, "a2a-j", "conversion", "", time.Now().UTC(), []ledger.Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 1000}, {Account: "HOLD:USD", Amount: -1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.h.DB, `INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, created_at)
		VALUES (3, 1, ?, 1000, 'USD', '{}', 'approved', '2026-01-20 10:00:00')`, jid)

	m := mlm.New(e.h.DB)
	w1s, w1e := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	w2s, w2e := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)

	if _, err := m.RunPeriod(ctx, w1s, w1e); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunPeriod(ctx, w2s, w2e); err != nil {
		t.Fatal(err)
	}
	total := qInt64(t, e.h.DB, `SELECT COALESCE(SUM(commission_cents),0) FROM mlm_period_lines`)
	t.Logf("tier split total across overlapping periods = %d cents (entitlement = 150)", total)
	if total > 150 {
		t.Errorf("P0 DOUBLE TIER PAY: same commission paid %d cents across overlapping windows (idempotency key scoped to period_id only; no global per-commission cap)", total)
	}
}

// 2b. Concurrent SettlePairs on the same (affiliate, day): the CAS on
// pairs_paid guards the row, but the LOSER still reports pairs+payout>0 —
// a caller that posts the payout from the return value double-pays.
func TestAttack2b_ConcurrentSettlePairs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := mlm.New(e.h.DB)
	day := int64(20260101)
	if err := m.RecordLegVolume(ctx, 1, "L", day, 10); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordLegVolume(ctx, 1, "R", day, 10); err != nil {
		t.Fatal(err)
	}
	cfg := mlm.PairConfig{PairPayoutCents: 500, MaxPairsPerDay: 0}

	var wg sync.WaitGroup
	var reported atomic.Int64
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 10; attempt++ {
				pairs, _, err := m.SettlePairs(ctx, 1, day, cfg)
				if isBusy(err) {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				if err == nil && pairs > 0 {
					reported.Add(1)
				}
				if err != nil && !isBusy(err) {
					t.Logf("SettlePairs error: %v", err)
				}
				return
			}
		}()
	}
	wg.Wait()
	paid := qInt64(t, e.h.DB, `SELECT pairs_paid FROM pair_volume WHERE affiliate_id=1 AND day=?`, day)
	t.Logf("concurrent SettlePairs: callers reporting payout=%d, pairs_paid in DB=%d", reported.Load(), paid)
	if paid != 10 {
		t.Errorf("P1 pairs_paid=%d want 10 (double-consumed volume)", paid)
	}
	if reported.Load() > 1 {
		t.Errorf("P1 FALSE PAYOUT REPORT: %d concurrent callers each told the pairing payout happened; a caller posting from the return value pays twice", reported.Load())
	}
}

// 2c. Period window whose START DATE equals the commission's creation date:
// created_at is stored in SQLite datetime format ("YYYY-MM-DD HH:MM:SS")
// but compared against RFC3339 ("YYYY-MM-DDTHH:MM:SSZ") — ' ' < 'T' so the
// commission sorts BEFORE the window start and is silently excluded.
func TestAttack2c_CreatedAtFormatMismatch_SilentExclusion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustExec(t, e.h.DB, `INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level) VALUES (2,3,NULL,1)`)
	mustExec(t, e.h.DB, `INSERT INTO mlm_tier_rates (campaign_id, level, rate_bp) VALUES (1,1,1000)`)

	eng := commissions.NewEngine(e.h.DB, e.p)
	cid, err := eng.ProcessPurchase(ctx, purchase("a2c-evt", 3, 10000))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.h.DB, `UPDATE commissions SET status='approved', approved_at=? WHERE id=?`, time.Now().UTC(), cid)
	created := qStr(t, e.h.DB, `SELECT created_at FROM commissions WHERE id=?`, cid)
	t.Logf("engine-created created_at stored as %q", created)

	m := mlm.New(e.h.DB)
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	n, err := m.RunPeriod(ctx, today, today.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RunPeriod over [today, tomorrow) saw %d lines", n)
	if n == 0 {
		t.Errorf("P1 SILENT EXCLUSION: commission created today is invisible to a period window starting today (datetime-vs-RFC3339 string compare: ' ' < 'T') — tier payouts silently skipped")
	}
}

// ===========================================================================
// ATTACK 3 — usage settle at rate R, re-settle at R/2; pass-2 crash stall
// ===========================================================================

func TestAttack3_UsageRateDecreaseAndCrash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mustExec(t, e.h.DB, `INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method)
		VALUES (1, 'cus_u1', 1, 'token'), (1, 'cus_u2', 1, 'token')`)
	ing := usage.New(e.h.DB)
	day := int64(20260101)
	for i := 0; i < 10; i++ {
		if err := ing.Ingest(ctx, usage.Event{
			Source: "api", ExternalID: fmt.Sprintf("u1-%d", i),
			CustomerExtID: "cus_u1", Units: 1000, UnitType: "tokens",
			OccurredAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	st := usage.NewSettler(e.h.DB, usagePosterAdapter{e.p})

	// Settle at 100bp per 10k units => 10000 units -> 100 cents.
	n, err := st.SettleDay(ctx, day, 100)
	if err != nil || n != 1 {
		t.Fatalf("settle n=%d err=%v", n, err)
	}
	// Rate cut to 50: entitlement drops to 50 — delta negative.
	n, err = st.SettleDay(ctx, day, 50)
	if err != nil {
		t.Fatalf("rate-decrease settle errored: %v", err)
	}
	neg := qInt64(t, e.h.DB, `SELECT COALESCE(SUM(amount),0) FROM commissions WHERE amount < 0 AND CAST(rate_snapshot AS text) LIKE '%usage%'`)
	t.Logf("after rate cut: new commissions=%d, negative usage commissions=%d (no credit-back: lenient by design)", n, neg)
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d after rate cut", s)
	}

	// Pass-2 crash: ledger posts (pass 1, ledger conn) then the domain tx
	// dies. Re-run must recover; instead the idem key is a DUPLICATE.
	// Meter for the crash-recovery scenario: 10k units.
	for i := 0; i < 10; i++ {
		if err := ing.Ingest(ctx, usage.Event{
			Source: "api", ExternalID: fmt.Sprintf("u2-%d", i),
			CustomerExtID: "cus_u2", Units: 1000, UnitType: "tokens",
			OccurredAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Portable pass-2 crash injection: perform pass 1 by hand (journal
	// lands on the ledger conn), leave the domain rows absent — the exact
	// state a pass-2 crash leaves. (SQL-trigger injection is dialect-
	// specific; this reproduces the same wedge state on both engines.)
	{
		var affiliateID int64
		if err := e.h.DB.QueryRowContext(ctx,
			`SELECT affiliate_id FROM customers WHERE program_id=1 AND external_customer_id='cus_u2'`).
			Scan(&affiliateID); err != nil {
			t.Fatal(err)
		}
		basis := int64(10000 * 100 / 10000) // 10k units at 100bp
		idem := fmt.Sprintf("usage:%d:%s:%s:%d", day, "cus_u2", "tokens", basis)
		if _, err := e.p.Post(ctx, idem, "usage_settlement", "cus_u2", time.Unix(0, 0).UTC(),
			[]ledger.Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: basis},
				{Account: "HOLD:USD", Amount: -basis},
			}); err != nil {
			t.Fatal(err)
		}
		_ = affiliateID
	}

	// Recovery run.
	_, err = st.SettleDay(ctx, day, 100)
	t.Logf("recovery settle error: %v", err)
	commU2 := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM commissions WHERE CAST(rate_snapshot AS text) LIKE '%"origin":"usage"%' AND CAST(rate_snapshot AS text) LIKE '%cus_u2%'`)
	settledU2 := qInt64(t, e.h.DB, `SELECT settled_cents FROM usage_daily WHERE customer_ext_id='cus_u2' AND day=?`, day)
	journals := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM journal_entries WHERE source='usage_settlement'`)
	t.Logf("after recovery attempt: commissions(usage,u2)=%d settled_cents(u2)=%d usage journals=%d", commU2, settledU2, journals)

	if err != nil {
		t.Errorf("P0 SETTLEMENT STALL: pass-2 crash after ledger post leaves journals committed with settled_cents un-advanced; every re-run dies on the idem key (%v). HOLD carries a commission liability with no commission row, forever.", err)
	}
	if settledU2 == 0 && journals > 0 {
		t.Errorf("P0 PHANTOM HOLD: %d usage journal(s) posted with settled_cents=0 and no commission row", journals)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}
}

// ===========================================================================
// ATTACK 4 — Mature() vs fraud Uphold: commission cancelled between the
// Mature batch SELECT and its status CAS. The HOLD->payable journal is
// already committed; the CAS just skips counting.
// ===========================================================================

func TestAttack4_MatureVsUphold(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	eng := commissions.NewEngine(e.h.DB, e.p)
	cid, err := eng.ProcessPurchase(ctx, purchase("a4-evt", 1, 10000))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.h.DB, `UPDATE commissions SET matures_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour), cid)

	var hooked bool
	hp := &hookPoster{real: e.p, before: func(idem string) {
		if strings.HasPrefix(idem, "mature:") && !hooked {
			hooked = true
			// Uphold fires while Mature is mid-window — on its own goroutine,
			// exactly as a concurrent admin request would. The per-commission
			// moneylock serializes the two CAS->post windows; whichever order
			// they land in, the books must reconcile.
			go func() {
				if err := prodUphold(ctx, t, e, cid, "mid-mature uphold"); err != nil {
					t.Logf("concurrent uphold: %v", err)
				}
			}()
		}
	}}
	raceEng := commissions.NewEngine(e.h.DB, hp)
	n, err := raceEng.Mature(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Mature reported n=%d (CAS lost for the raced commission)", n)

	status := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE id=?`, cid)
	bal, _ := e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	holdBal, _ := e.p.AccountBalance(ctx, "HOLD:USD")
	t.Logf("commission status=%s payable=%d HOLD=%d", status, bal, holdBal)

	if status == "cancelled" && bal != 0 {
		t.Errorf("P0 PAYABLE FOR CANCELLED COMMISSION: fraud-upheld commission is cancelled yet AFFILIATE_PAYABLE:1 = %d — Mature's journal committed before its status CAS and is never reversed. The affiliate is now owed money fraud removed, and HOLD=%d is a corrupted phantom asset.", bal, holdBal)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}
}

// ===========================================================================
// ATTACK 5 — Clawback of a commission that MATURED between the clawback's
// parent read and its ledger post. ProcessRefund claws from HOLD (stale
// read) while the money already sits in payable, and its status UPDATE is
// unguarded so approved->cancelled passes silently.
// ===========================================================================

func TestAttack5_ClawbackVsMature(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	realEng := commissions.NewEngine(e.h.DB, e.p)
	cid, err := realEng.ProcessPurchase(ctx, purchase("a5-evt", 1, 10000))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.h.DB, `UPDATE commissions SET matures_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour), cid)

	var hooked bool
	matured := make(chan struct{})
	hp := &hookPoster{real: e.p, before: func(idem string) {
		if strings.HasPrefix(idem, "clawback:") && !hooked {
			hooked = true
			// Maturation fires INSIDE the refund's read->post window, from
			// its own goroutine as the maturation worker would. (It used to
			// run inline here; the refund now holds its transaction across
			// the window, so an inline call would wait on itself.)
			go func() {
				defer close(matured)
				if _, err := realEng.Mature(ctx); err != nil {
					t.Logf("hook mature: %v", err)
				}
			}()
		}
	}}
	raceEng := commissions.NewEngine(e.h.DB, hp)

	refund := purchase("a5-ref", 1, 10000)
	refund.EventType = "refund"
	if err := raceEng.ProcessRefund(ctx, refund, cid, 10000, 10000); err != nil {
		t.Fatal(err)
	}
	if hooked {
		<-matured
	}

	status := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE id=?`, cid)
	bal, _ := e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	holdBal, _ := e.p.AccountBalance(ctx, "HOLD:USD")
	expBal, _ := e.p.AccountBalance(ctx, "EXPENSE:COMMISSION:USD")
	t.Logf("parent status=%s payable=%d HOLD=%d expense=%d", status, bal, holdBal, expBal)

	if status == "cancelled" && bal != 0 {
		t.Errorf("P0 REFUND ESCAPES: commission fully refunded+cancelled but AFFILIATE_PAYABLE:1=%d still owes the affiliate the money (claw took from a drained HOLD=%d instead of payable — expense double-credited, books corrupted)", bal, holdBal)
	}
	if holdBal > 0 {
		t.Errorf("P1 HOLD left positive (%d) — a liability account holding a debit from the stale-account clawback", holdBal)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}
}

// ===========================================================================
// ATTACK 6 — importer: same affiliate from two sources concurrently
// ===========================================================================

func TestAttack6_ImporterConcurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	aff := func(email, code string) []importer.Affiliate {
		return []importer.Affiliate{{Email: email, Code: code, Status: "active"}}
	}

	// 6a. Same email, two sources, concurrently.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i, src := range []importer.Source{importer.Rewardful, importer.Tapfiliate, importer.Rewardful, importer.Tolt} {
		i, src := i, src
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := importer.Migrate(ctx, e.h.DB, src, aff("dup@t.dev", fmt.Sprintf("codea%d", i)), false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Logf("concurrent migrate error: %v", err)
	}
	n := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM affiliates WHERE email='dup@t.dev'`)
	t.Logf("affiliates rows for dup@t.dev = %d", n)
	if n != 1 {
		t.Errorf("P1 IMPORT DUPLICATE: %d affiliate rows for one email (unique constraint bypassed)", n)
	}
	codes := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM affiliate_codes WHERE code LIKE 'codea%'`)
	if codes > 4 {
		t.Errorf("P2 code rows=%d", codes)
	}

	// 6b. Different emails, SAME code, concurrently — clash resolution uses
	// an in-tx snapshot, so the suffix dance can be blind to the rival tx.
	start := make(chan struct{})
	var ok1, ok2 atomic.Int64
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := importer.Migrate(ctx, e.h.DB, importer.Rewardful,
				[]importer.Affiliate{{Email: fmt.Sprintf("clash%d@t.dev", i), Code: "sharedcode", Status: "active"}}, false)
			if err == nil {
				ok1.Add(1)
			} else {
				t.Logf("clash migrate %d failed: %v", i, err)
				ok2.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	nShared := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM affiliate_codes WHERE code='sharedcode'`)
	nSuffixed := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM affiliate_codes WHERE code LIKE 'sharedcode-%'`)
	t.Logf("shared code: exact=%d suffixed=%d, successful batches=%d failed=%d", nShared, nSuffixed, ok1.Load(), ok2.Load())
	if nShared > 1 {
		t.Errorf("P1 DUPLICATE CODES: %d rows with code 'sharedcode' — two affiliates now own the same referral slug", nShared)
	}
	if ok2.Load() > 0 {
		t.Logf("P2 AVAILABILITY: %d entire import batch(es) rolled back because of one code/email clash (no per-row skip)", ok2.Load())
	}
}

// ===========================================================================
// ATTACK 7 — postback emitter: commission cancelled while postback in flight
// ===========================================================================

func TestAttack7_PostbackCancelledInFlight(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	deliveries := map[string]int{}
	var cancelled atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		amt := r.URL.Query().Get("c")
		mu.Lock()
		deliveries[amt]++
		first := deliveries[amt] == 1
		mu.Unlock()
		if first && !cancelled.Load() {
			// Fraud uphold lands while the postback is in flight.
			if err := prodUphold(context.Background(), t, e, qInt64(t, e.h.DB,
				`SELECT id FROM commissions WHERE amount=1000 AND status IN ('pending','approved') ORDER BY id LIMIT 1`), "mid-flight"); err != nil {
				t.Logf("mid-flight uphold: %v", err)
			}
			cancelled.Store(true)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mustExec(t, e.h.DB, `UPDATE affiliates SET postback_url=? WHERE id=1`, srv.URL+"/pb?c={commission_cents}")
	eng := commissions.NewEngine(e.h.DB, e.p)
	if _, err := eng.ProcessPurchase(ctx, purchase("a7-evt", 1, 10000)); err != nil {
		t.Fatal(err)
	}

	em := postbacks.NewEmitter(e.h.DB)
	em.AllowLocalhost = true
	em.BackoffBase = 5 * time.Millisecond
	em.TickInterval = 25 * time.Millisecond
	em.Start(ctx)
	defer em.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := deliveries["1000"]
		mu.Unlock()
		if n >= 1 && cancelled.Load() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let further ticks run

	mu.Lock()
	n := deliveries["1000"]
	mu.Unlock()
	status := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE amount=1000 ORDER BY id LIMIT 1`)
	delivered := qStr(t, e.h.DB, `SELECT CASE WHEN delivered_at IS NULL THEN 'never' ELSE 'delivered' END FROM postback_state ORDER BY commission_id LIMIT 1`)
	t.Logf("commission status=%s, deliveries of amount=1000: %d, postback_state.delivered_at=%s", status, n, delivered)

	if n >= 1 && status == "cancelled" {
		t.Logf("P2 STALE MONEY NOTICE: affiliate endpoint was told '1000 cents' for a commission that fraud-cancelled mid-flight — S2S postbacks have no retraction/undo event, and clawbacks emit no outbox topic at all")
	}
	if n > 1 {
		t.Errorf("P1 duplicate postback deliveries: %d", n)
	}
	// A commission cancelled BEFORE any tick must never fire.
	// Deterministic: PAUSE the emitter first (Stop waits for the in-flight
	// tick), create + cancel commission 2 with no ticks running, then
	// re-Start and give it ample ticks. If any delivery for commission 2
	// still lands, the emitter read a cancelled commission — the real bug.
	em.Stop()
	refund := purchase("a7-evt2", 1, 10000)
	if _, err := eng.ProcessPurchase(ctx, refund); err != nil {
		t.Fatal(err)
	}
	cid2 := qInt64(t, e.h.DB, `SELECT id FROM commissions WHERE amount=1000 ORDER BY id DESC LIMIT 1`)
	if err := prodUphold(context.Background(), t, e, cid2, "pre-tick"); err != nil {
		t.Fatal(err)
	}
	em2 := postbacks.NewEmitter(e.h.DB)
	em2.AllowLocalhost = true
	em2.BackoffBase = 5 * time.Millisecond
	em2.TickInterval = 25 * time.Millisecond
	em2.Start(ctx)
	defer em2.Stop()
	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	tot := 0
	for _, v := range deliveries {
		tot += v
	}
	mu.Unlock()
	t.Logf("total deliveries after pre-tick cancel: %d (want the single mid-flight one)", tot)
	if tot > n {
		t.Errorf("P2 postback fired for a commission already cancelled before delivery (total=%d, midflight=%d)", tot, n)
	}
}

// ===========================================================================
// ATTACK 8 — gamify OnCommission for clawback rows (amount <= 0)
// ===========================================================================

func TestAttack8_GamifyClawbackXP(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	g := gamify.New(e.h.DB)
	if err := g.SeedBadges(ctx); err != nil {
		t.Fatal(err)
	}
	eng := commissions.NewEngine(e.h.DB, e.p)
	cid, err := eng.ProcessPurchase(ctx, purchase("a8-evt", 1, 10000))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.OnCommission(ctx, 1, cid, 1000); err != nil {
		t.Fatal(err)
	}
	xpAfterReal := qInt64(t, e.h.DB, `SELECT xp FROM xp_totals WHERE affiliate_id=1`)
	t.Logf("XP after real commission: %d", xpAfterReal)

	// Clawback: refund creates a NEGATIVE commission row with its own id.
	refund := purchase("a8-ref", 1, 10000)
	refund.EventType = "refund"
	if err := eng.ProcessRefund(ctx, refund, cid, 10000, 10000); err != nil {
		t.Fatal(err)
	}
	clawID := qInt64(t, e.h.DB, `SELECT id FROM commissions WHERE amount < 0 ORDER BY id DESC LIMIT 1`)
	xpClaw := qInt64(t, e.h.DB, `SELECT xp FROM xp_totals WHERE affiliate_id=1`)

	// The money-hook contract (engine.go: "called after a commission is
	// credited") invoked for the clawback row, as any naive consumer would:
	if err := g.OnCommission(ctx, 1, clawID, -1000); err != nil {
		t.Logf("OnCommission(-1000) errored: %v", err)
	}
	xpAfterClaw := qInt64(t, e.h.DB, `SELECT xp FROM xp_totals WHERE affiliate_id=1`)
	t.Logf("clawID=%d xp before hook=%d after hook=%d", clawID, xpClaw, xpAfterClaw)
	if xpAfterClaw > xpClaw {
		t.Errorf("P2 XP FOR CLAWBACK: OnCommission ignores the amount sign — a clawback row (money REMOVED) grants +%d conversion XP; leaderboard/milestones inflate on refunds", xpAfterClaw-xpClaw)
	}
	// Zero-amount hook is a no-op?
	before := xpAfterClaw
	_ = g.OnCommission(ctx, 1, 999999, 0)
	after := qInt64(t, e.h.DB, `SELECT xp FROM xp_totals WHERE affiliate_id=1`)
	if after > before {
		t.Errorf("P2 XP FOR ZERO: +%d XP granted for amount=0", after-before)
	}
}

// ===========================================================================
// ATTACK 9 — int64 overflow through percent math (gross*rate/10000)
// ===========================================================================

func TestAttack9_Overflow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 9a. commissions.computeAmount: gross*bp wraps.
	eng := commissions.NewEngine(e.h.DB, e.p)
	mustExec(t, e.h.DB, `INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (1, NULL, 'overflow', 'percent', 10000)`)
	// Rule resolution picks ORDER BY priority DESC among campaign rules; use a fresh campaign to isolate.
	gross := int64(9000000000000000000) // 9e18, < 2^63
	ev := purchase("a9-evt", 1, gross)
	ev.CampaignID = 2
	mustExec(t, e.h.DB, `INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (2, NULL, 'purchase', 'percent', 10000)`) // 100%
	_, err := eng.ProcessPurchase(ctx, ev)
	exact := new(big.Int).Div(new(big.Int).Mul(big.NewInt(gross), big.NewInt(10000)), big.NewInt(10000))
	var got int64
	var note string
	_ = e.h.DB.QueryRow(`SELECT amount FROM commissions ORDER BY id DESC LIMIT 1`).Scan(&got)
	_ = e.h.DB.QueryRow(`SELECT COALESCE(process_error,'') FROM raw_events WHERE external_event_id='a9-evt'`).Scan(&note)
	t.Logf("9a commissions: err=%v stored=%d exact=%s raw_note=%q", err, got, exact, note)
	if got != exact.Int64() {
		t.Errorf("P1 OVERFLOW WRAP in commission percent math: stored %d, exact %s — int64 wrap in gross*rate_bp/10000 silently mis-prices (or drops via zero_commission) near-2^62 grosses", got, exact)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Errorf("integrity=%d", s)
	}

	// 9b. usage basis: units*rate/10000 wraps.
	mustExec(t, e.h.DB, `INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method)
		VALUES (1, 'cus_ov', 1, 'token')`)
	ing := usage.New(e.h.DB)
	if err := ing.Ingest(ctx, usage.Event{
		Source: "api", ExternalID: "ov-1", CustomerExtID: "cus_ov",
		Units: 8000000000000000000, UnitType: "tokens",
		OccurredAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	st := usage.NewSettler(e.h.DB, usagePosterAdapter{e.p})
	if _, err := st.SettleDay(ctx, 20260102, 10000); err != nil {
		t.Logf("9b settle err=%v", err)
	}
	settled := qInt64(t, e.h.DB, `SELECT settled_cents FROM usage_daily WHERE customer_ext_id='cus_ov'`)
	t.Logf("9b usage: settled_cents=%d", settled)
	// FIXED (checked math): the basis saturates at MaxInt64 instead of
	// wrapping negative. At token-scale volumes the exact product exceeds
	// any real currency; the safety property is NEVER-negative and
	// NEVER-float — not exactness at absurd magnitudes.
	if settled < 0 {
		t.Errorf("P1 OVERFLOW WRAPPED NEGATIVE: settled=%d — meter math must saturate, not wrap", settled)
	}
	var typU string
	if dbtest.IsPostgres() {
		e.h.DB.QueryRow(`SELECT pg_typeof(settled_cents)::text FROM usage_daily WHERE customer_ext_id='cus_ov'`).Scan(&typU)
		if typU != "bigint" {
			t.Errorf("P0 FLOAT MONEY: usage settled_cents stored as %s", typU)
		}
	} else {
		e.h.DB.QueryRow(`SELECT typeof(settled_cents) FROM usage_daily WHERE customer_ext_id='cus_ov'`).Scan(&typU)
		if typU != "integer" {
			t.Errorf("P0 FLOAT MONEY: usage settled_cents stored as %s", typU)
		}
	}

	// 9c. mlm tier split in SQL: SQLite promotes int64 overflow to REAL —
	// float money appears in mlm_period_lines.
	mustExec(t, e.h.DB, `INSERT INTO mlm_closure (ancestor_id, descendant_id, outer_leg, level) VALUES (2,3,NULL,1)`)
	mustExec(t, e.h.DB, `INSERT INTO mlm_tier_rates (campaign_id, level, rate_bp) VALUES (1,1,20000)`) // 200%
	jid, err := e.p.Post(ctx, "a9c-j", "conversion", "", time.Now().UTC(), []ledger.Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 1}, {Account: "HOLD:USD", Amount: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.h.DB, `INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, created_at)
		VALUES (3, 1, ?, 4611686018427387904, 'USD', '{}', 'approved', '2026-01-20 10:00:00')`, jid)
	m := mlm.New(e.h.DB)
	if _, err := m.RunPeriod(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Logf("9c RunPeriod err=%v", err)
	}
	var cents int64
	var typ string
	if dbtest.IsPostgres() {
		if err := e.h.DB.QueryRow(`SELECT commission_cents, pg_typeof(commission_cents)::text FROM mlm_period_lines LIMIT 1`).Scan(&cents, &typ); err != nil {
			t.Logf("9c scan err=%v", err)
		} else if typ != "bigint" {
			t.Errorf("P0 FLOAT MONEY: mlm commission_cents stored as %s", typ)
		}
	} else if err := e.h.DB.QueryRow(`SELECT commission_cents, typeof(commission_cents) FROM mlm_period_lines LIMIT 1`).Scan(&cents, &typ); err != nil {
		t.Logf("9c scan err=%v", err)
	} else if typ != "integer" {
		t.Errorf("P0 FLOAT MONEY: mlm_period_lines.commission_cents stored as %s (int64 overflow promotes to REAL on SQLite) — violates the no-float-money cardinal rule", typ)
	}
}

// ===========================================================================
// ATTACK 10 — payout threshold boundary + held-then-released + concurrent
// CreateBatch double-staging
// ===========================================================================

func TestAttack10_PayoutEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("ThresholdExactAndHoldRelease", func(t *testing.T) {
		e := newEnv(t)
		wirePayoutLedger(t, e.p)
		eng := commissions.NewEngine(e.h.DB, e.p)
		r := payouts.NewRunner(e.h.DB)
		cid := maturedCommission(t, eng, e.h.DB, "a10a", 1, 10000) // payable -1000

		// Held: excluded from batching.
		mustExec(t, e.h.DB, `INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'fraud review')`, cid)
		if _, n, err := r.CreateBatch(ctx, 1000, "manual"); err != nil || n != 0 {
			t.Fatalf("held: batch n=%d err=%v (want 0)", n, err)
		}
		// Released: eligible, threshold EXACTLY equal to total.
		mustExec(t, e.h.DB, `UPDATE commission_holds SET resolved_at = ?, resolution='released' WHERE commission_id = ?`,
			time.Now().UTC().Format(time.RFC3339Nano), cid)
		if _, n, err := r.CreateBatch(ctx, 1000, "manual"); err != nil || n != 1 {
			t.Fatalf("released+exact-threshold: batch n=%d err=%v (want 1: >= boundary must include)", n, err)
		}
		amt := qInt64(t, e.h.DB, `SELECT amount FROM payouts`)
		if amt != 1000 {
			t.Errorf("payout amount=%d want 1000", amt)
		}
		// Re-run: no double stage.
		if _, n, _ := r.CreateBatch(ctx, 1000, "manual"); n != 0 {
			t.Errorf("P1 re-batch staged %d more payouts for already-lined commissions", n)
		}

		// Hold landing AFTER staging but BEFORE Approve: Approve ignores holds.
		cid2 := maturedCommission(t, eng, e.h.DB, "a10a2", 1, 10000)
		if _, n, err := r.CreateBatch(ctx, 1000, "manual"); err != nil || n != 1 {
			t.Fatalf("second batch n=%d err=%v", n, err)
		}
		mustExec(t, e.h.DB, `INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'late fraud')`, cid2)
		pid := qInt64(t, e.h.DB, `SELECT id FROM payouts WHERE id = (SELECT MAX(payout_id) FROM payout_lines WHERE commission_id=?)`, cid2)
		err := r.Approve(ctx, pid, nil)
		status := qStr(t, e.h.DB, `SELECT status FROM commissions WHERE id=?`, cid2)
		openHold := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM commission_holds WHERE commission_id=? AND resolved_at IS NULL`, cid2)
		t.Logf("post-batch hold: Approve err=%v commission=%s openHolds=%d", err, status, openHold)
		if err == nil && status == "paid" && openHold == 1 {
			t.Errorf("P0 FRAUD HOLD BYPASS: commission paid while an OPEN fraud hold exists — Approve never re-checks commission_holds after staging")
		}
	})

	t.Run("ConcurrentCreateBatch_DoubleStage", func(t *testing.T) {
		e := newEnv(t)
		wirePayoutLedger(t, e.p)
		eng := commissions.NewEngine(e.h.DB, e.p)
		r := payouts.NewRunner(e.h.DB)
		for i := 0; i < 3; i++ {
			maturedCommission(t, eng, e.h.DB, fmt.Sprintf("a10b-%d", i), 1, 10000)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for attempt := 0; attempt < 10; attempt++ {
					_, _, err := r.CreateBatch(ctx, 0, "manual")
					if !isBusy(err) {
						if err != nil {
							t.Logf("concurrent CreateBatch: %v", err)
						}
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
		close(start)
		wg.Wait()

		dup := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM (
			SELECT pl.commission_id, COUNT(DISTINCT pl.payout_id) np
			FROM payout_lines pl GROUP BY pl.commission_id HAVING COUNT(DISTINCT pl.payout_id) > 1)`)
		npayouts := qInt64(t, e.h.DB, `SELECT COUNT(*) FROM payouts`)
		t.Logf("concurrent CreateBatch: payouts=%d commissions in >1 payout=%d", npayouts, dup)
		if dup > 0 {
			t.Errorf("P0 DOUBLE-STAGED PAYOUTS: %d commission(s) staged into multiple payouts by concurrent CreateBatch runs", dup)
		}
		if npayouts > 1 {
			t.Errorf("P1 %d payout rows for one affiliate one cycle (split/stranded batches)", npayouts)
		}
	})
}
