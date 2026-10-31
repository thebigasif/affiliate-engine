package commissions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

func refundEvent(id string) commissions.Event {
	ev := purchase(id, 0)
	ev.EventType = "refund"
	return ev
}

// failOncePoster fails the first Post whose idem key has the prefix.
// It has no PostClawback, so clawbacks go through the guarded Post.
type failOncePoster struct {
	commissions.Poster
	prefix string
	failed bool
}

func (f *failOncePoster) Post(ctx context.Context, idem, source, desc string, eff time.Time, lines []ledger.Line) (int64, error) {
	if !f.failed && strings.HasPrefix(idem, f.prefix) {
		f.failed = true
		return 0, fmt.Errorf("injected ledger failure")
	}
	return f.Poster.Post(ctx, idem, source, desc, eff, lines)
}

func parentStatus(t *testing.T, e *env, cid int64) string {
	t.Helper()
	var s string
	if err := e.h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func clawRows(t *testing.T, e *env, cid int64) (n int, sum int64) {
	t.Helper()
	if err := e.h.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM commissions
		WHERE parent_commission_id = ?`, cid).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

// Fix 1: a $0 (or negative) gross never pays a bounty — a fixed-rate rule
// used to pay its full RateValue on a $0 trial invoice. The event is
// recorded and ACKed (nil error, never blocks the webhook).
func TestCommission_ZeroGrossNoBounty(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		VALUES (1, 1, 'purchase', 'fixed', 500)`); err != nil {
		t.Fatal(err)
	}
	for _, gross := range []int64{0, -100} {
		id := fmt.Sprintf("zero%d", gross)
		cid, err := e.e.ProcessPurchase(ctx, purchase(id, gross))
		if err != nil || cid != 0 {
			t.Fatalf("gross=%d: cid=%d err=%v (want no-op)", gross, cid, err)
		}
		var note string
		if err := e.h.DB.QueryRow(`SELECT process_error FROM raw_events
			WHERE external_event_id = ?`, id).Scan(&note); err != nil {
			t.Fatal(err)
		}
		if note != "non_positive_gross" {
			t.Fatalf("gross=%d: note=%q want non_positive_gross", gross, note)
		}
		// Replay of the skipped event is a finished event.
		if _, err := e.e.ProcessPurchase(ctx, purchase(id, gross)); err != commissions.ErrAlreadyProcessed {
			t.Fatalf("gross=%d replay: %v", gross, err)
		}
	}
	var n, j int
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&j)
	if n != 0 || j != 0 {
		t.Fatalf("commissions=%d journals=%d want 0/0 (no bounty on $0)", n, j)
	}
	// A paid purchase still earns the fixed bounty.
	cid, err := e.e.ProcessPurchase(ctx, purchase("paid-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 500 {
		t.Fatalf("amount=%d want 500 (fixed bounty on a real sale)", got)
	}
}

// Fix 2: a refund after payout claws back into a POSITIVE payable (the
// affiliate owes us) instead of tripping the overdraft guard and losing
// the clawback.
func TestRefund_ClawbackAfterPayout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.p.EnsureAccounts(ctx, map[string]string{"PAYOUT_CLEARING:USD": "clearing"}); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, purchase("cap-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	e.h.DB.Exec(`UPDATE commissions SET matures_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), cid)
	if _, err := e.e.Mature(ctx); err != nil {
		t.Fatal(err)
	}
	// Paid out: payable -> clearing, commission -> paid.
	if _, err := e.p.Post(ctx, "payout:cap", "payout", "", time.Now().UTC(), []ledger.Line{
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: 1000},
		{Account: "PAYOUT_CLEARING:USD", Amount: -1000},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`UPDATE commissions SET status='paid', paid_at=? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), cid); err != nil {
		t.Fatal(err)
	}

	if err := e.e.ProcessRefund(ctx, refundEvent("cap-ref"), cid, 10000, 10000); err != nil {
		t.Fatalf("post-payout clawback lost: %v", err)
	}
	if s := parentStatus(t, e, cid); s != "clawback" {
		t.Fatalf("parent=%s want clawback", s)
	}
	if n, sum := clawRows(t, e, cid); n != 1 || sum != -1000 {
		t.Fatalf("clawback rows=%d sum=%d want 1/-1000", n, sum)
	}
	if bal := holdBalance(t, e.h.DB, "AFFILIATE_PAYABLE:1:USD"); bal != 1000 {
		t.Fatalf("payable=%d want +1000 (affiliate owes us)", bal)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// Fix 3: a purchase whose ledger post failed is retried by the provider
// under the SAME event id — and now completes instead of being refused
// as already processed.
func TestPurchase_FailedPostRetries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	flaky := commissions.NewEngine(e.h.DB, &failOncePoster{Poster: e.p, prefix: "commission:"})
	ev := purchase("fp-1", 10000)
	if _, err := flaky.ProcessPurchase(ctx, ev); err == nil {
		t.Fatal("expected injected ledger failure")
	}
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil || cid == 0 {
		t.Fatalf("retry: cid=%d err=%v", cid, err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("amount=%d want 1000", got)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != -1000 {
		t.Fatalf("HOLD=%d want -1000", hold)
	}
	if _, err := e.e.ProcessPurchase(ctx, ev); err != commissions.ErrAlreadyProcessed {
		t.Fatalf("replay after success: %v", err)
	}
}

// Fix 3: a refund whose claw post failed AFTER the parent's terminal CAS
// is retried under the same event id and claws from the right account.
func TestRefund_FailedClawRetries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("rr-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	flaky := commissions.NewEngine(e.h.DB, &failOncePoster{Poster: e.p, prefix: "clawback:"})
	ref := refundEvent("rr-ref")
	if err := flaky.ProcessRefund(ctx, ref, cid, 10000, 10000); err == nil {
		t.Fatal("expected injected ledger failure")
	}
	if err := e.e.ProcessRefund(ctx, ref, cid, 10000, 10000); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s := parentStatus(t, e, cid); s != "cancelled" {
		t.Fatalf("parent=%s want cancelled", s)
	}
	if n, sum := clawRows(t, e, cid); n != 1 || sum != -1000 {
		t.Fatalf("clawback rows=%d sum=%d want 1/-1000", n, sum)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("HOLD=%d want 0 (claw landed on retry)", hold)
	}
	if err := e.e.ProcessRefund(ctx, ref, cid, 10000, 10000); err != commissions.ErrAlreadyProcessed {
		t.Fatalf("replay after success: %v", err)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// Fix 4: partial refunds each claw their share; the parent only reaches
// its terminal status once fully refunded.
func TestRefund_PartialRefundsAccumulate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("pr-1", 10000)) // 1000 commission
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		id         string
		refunded   int64
		wantSum    int64
		wantStatus string
	}{
		{"pr-r1", 3000, -300, "pending"},
		{"pr-r2", 3000, -600, "pending"},
		{"pr-r3", 4000, -1000, "cancelled"},
	} {
		if err := e.e.ProcessRefund(ctx, refundEvent(s.id), cid, s.refunded, 10000); err != nil {
			t.Fatalf("%s: %v", s.id, err)
		}
		if _, sum := clawRows(t, e, cid); sum != s.wantSum {
			t.Fatalf("%s: clawed=%d want %d", s.id, sum, s.wantSum)
		}
		if st := parentStatus(t, e, cid); st != s.wantStatus {
			t.Fatalf("%s: parent=%s want %s", s.id, st, s.wantStatus)
		}
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("HOLD=%d want 0", hold)
	}
	// A refund after the parent is exhausted claws nothing.
	if err := e.e.ProcessRefund(ctx, refundEvent("pr-r4"), cid, 1000, 10000); err != nil {
		t.Fatal(err)
	}
	if n, sum := clawRows(t, e, cid); n != 3 || sum != -1000 {
		t.Fatalf("rows=%d sum=%d want 3/-1000", n, sum)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// Fix 4: cumulative proration is rounding-exact (thirds sum to the whole)
// and over-refunds clamp at the parent amount.
func TestRefund_PartialRoundingAndClamp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("rd-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range []int64{3333, 3333, 3334} {
		if err := e.e.ProcessRefund(ctx, refundEvent(fmt.Sprintf("rd-r%d", i)), cid, r, 10000); err != nil {
			t.Fatal(err)
		}
	}
	if n, sum := clawRows(t, e, cid); n != 3 || sum != -1000 {
		t.Fatalf("thirds: rows=%d sum=%d want 3/-1000", n, sum)
	}
	if st := parentStatus(t, e, cid); st != "cancelled" {
		t.Fatalf("parent=%s want cancelled", st)
	}

	// 60% + 60%: the second refund claws only the remaining 40%.
	cid2, err := e.e.ProcessPurchase(ctx, purchase("rd-2", 10000))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.e.ProcessRefund(ctx, refundEvent(fmt.Sprintf("rd2-r%d", i)), cid2, 6000, 10000); err != nil {
			t.Fatal(err)
		}
	}
	if n, sum := clawRows(t, e, cid2); n != 2 || sum != -1000 {
		t.Fatalf("clamp: rows=%d sum=%d want 2/-1000", n, sum)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("HOLD=%d want 0", hold)
	}
}

// Fix 4: a partially refunded pending commission matures only its
// unclawed remainder into payable.
func TestRefund_PartialThenMature(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("pm-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.e.ProcessRefund(ctx, refundEvent("pm-r1"), cid, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	e.h.DB.Exec(`UPDATE commissions SET matures_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), cid)
	if n, err := e.e.Mature(ctx); err != nil || n != 1 {
		t.Fatalf("mature n=%d err=%v", n, err)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("HOLD=%d want 0", hold)
	}
	if bal := holdBalance(t, e.h.DB, "AFFILIATE_PAYABLE:1:USD"); bal != -600 {
		t.Fatalf("payable=%d want -600 (1000 minus the 400 clawed while pending)", bal)
	}
	// A second partial refund on the now-approved parent claws payable.
	if err := e.e.ProcessRefund(ctx, refundEvent("pm-r2"), cid, 1000, 10000); err != nil {
		t.Fatal(err)
	}
	if bal := holdBalance(t, e.h.DB, "AFFILIATE_PAYABLE:1:USD"); bal != -500 {
		t.Fatalf("payable=%d want -500", bal)
	}
	if st := parentStatus(t, e, cid); st != "approved" {
		t.Fatalf("parent=%s want approved (partially refunded)", st)
	}
}

// Fix 4: providers reporting the CUMULATIVE refunded total (Lemon
// Squeezy) claw only the increment over what is already clawed.
func TestRefund_CumulativeProvider(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("cu-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		id         string
		cumulative int64
		wantSum    int64
	}{
		{"cu-r1", 2500, -250},
		{"cu-r2", 6000, -600},
		{"cu-r0", 2500, -600}, // late, older delivery: claws nothing
		{"cu-r3", 10000, -1000},
	} {
		if err := e.e.ProcessRefundCumulative(ctx, refundEvent(s.id), cid, s.cumulative, 10000); err != nil {
			t.Fatalf("%s: %v", s.id, err)
		}
		if _, sum := clawRows(t, e, cid); sum != s.wantSum {
			t.Fatalf("%s: clawed=%d want %d", s.id, sum, s.wantSum)
		}
	}
	if st := parentStatus(t, e, cid); st != "cancelled" {
		t.Fatalf("parent=%s want cancelled", st)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("HOLD=%d want 0", hold)
	}
}
