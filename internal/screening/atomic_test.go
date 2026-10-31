package screening_test

// The screening verdict and the hold it implies commit in the credit's own
// transaction, or the credit rolls back. Before, the hold was a separate
// write after the credit committed: a crash or a failed hold write between
// the two left a flagged commission unheld, and it matured and was paid
// like any other. A replayed delivery now repairs a commission an older
// release credited and never screened.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

// atomicEnv is realEnv plus the inner engine (to credit unscreened, as an
// older release did, and to mature).
func atomicEnv(t *testing.T, s screening.Screener, dryRun bool) (*db.Handles, *commissions.Engine, *screening.Engine) {
	t.Helper()
	h := dbtest.Open(t)
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'owner@gmail.com')`,
		`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value) VALUES (1, NULL, 'percent', 1000)`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	inner := commissions.NewEngine(h.DB, ledger.NewPoster(h.Ledger))
	if s == nil {
		s = fraud.NewEngine(h.DB)
	}
	return h, inner, screening.New(inner, s, dryRun)
}

// failHolds makes every commission_holds insert fail, on either engine.
func failHolds(t *testing.T, h *db.Handles) (restore func()) {
	t.Helper()
	stmts := []string{`CREATE TRIGGER t_fail_hold BEFORE INSERT ON commission_holds
		BEGIN SELECT RAISE(ABORT, 'injected hold failure'); END`}
	undo := []string{`DROP TRIGGER t_fail_hold`}
	if dbtest.Engine() == migrations.Postgres {
		stmts = []string{
			`CREATE FUNCTION t_fail_hold() RETURNS trigger LANGUAGE plpgsql AS
			 $$ BEGIN RAISE EXCEPTION 'injected hold failure'; END $$`,
			`CREATE TRIGGER t_fail_hold BEFORE INSERT ON commission_holds
			 FOR EACH ROW EXECUTE FUNCTION t_fail_hold()`,
		}
		undo = []string{`DROP TRIGGER t_fail_hold ON commission_holds`, `DROP FUNCTION t_fail_hold()`}
	}
	for _, q := range stmts {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, q := range undo {
			if _, err := h.DB.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func count(t *testing.T, h *db.Handles, q string, args ...any) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// mature moves every pending commission past its hold period.
func mature(t *testing.T, inner *commissions.Engine) {
	t.Helper()
	inner.SetClock(func() time.Time { return time.Now().Add(400 * 24 * time.Hour) })
	if _, err := inner.Mature(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// planned matures everything and returns what a payout run would pay now.
func planned(t *testing.T, h *db.Handles, inner *commissions.Engine) int64 {
	t.Helper()
	mature(t, inner)
	plan, err := payouts.NewRunner(h.DB).Preview(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range plan {
		total += p.Cents
	}
	return total
}

// A failed hold write used to be logged and swallowed: the webhook got
// 200, the self-referral commission stayed unheld, matured and was paid.
func TestScreening_HoldWriteFailureRollsBackCredit(t *testing.T) {
	h, inner, e := atomicEnv(t, nil, false)
	restore := failHolds(t, h)
	ev := purchase("hf-1", "owner+x@gmail.com", "")
	cid, err := e.ProcessPurchase(context.Background(), ev)
	if err == nil {
		held := count(t, h, `SELECT COUNT(*) FROM commission_holds WHERE commission_id = ?`, cid)
		t.Fatalf("hold write failed but the credit succeeded (commission %d, holds=%d); planned payout %d",
			cid, held, planned(t, h, inner))
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commissions`); n != 0 {
		t.Fatalf("credit committed without its hold: %d commissions", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM journal_entries`); n != 0 {
		t.Fatalf("journal committed without its hold: %d journals", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts`); n != 0 {
		t.Fatalf("verdicts committed without the credit: %d", n)
	}

	// The provider retries once the fault clears: credited and held.
	restore()
	cid, err = e.ProcessPurchase(context.Background(), ev)
	if err != nil || cid <= 0 {
		t.Fatalf("retry: cid=%d err=%v", cid, err)
	}
	if holdReason(t, h, cid) == "" {
		t.Fatal("retry credited without the hold")
	}
	if got := planned(t, h, inner); got != 0 {
		t.Fatalf("held commission planned into a payout of %d", got)
	}
}

// crashScreener stands in for a process that dies after the credit and
// before the hold: the panic unwinds through whatever is open.
type crashScreener struct{ *fraud.Engine }

func (crashScreener) Evaluate(context.Context, fraud.Query) ([]fraud.Verdict, error) {
	panic("crash between credit and hold")
}

func TestScreening_CrashBeforeHoldCommitsNothing(t *testing.T) {
	h, inner, _ := atomicEnv(t, nil, false)
	crashing := screening.New(inner, crashScreener{fraud.NewEngine(h.DB)}, false)
	ev := purchase("cr-1", "owner@gmail.com", "")
	func() {
		defer func() { _ = recover() }()
		_, _ = crashing.ProcessPurchase(context.Background(), ev)
	}()
	if n := count(t, h, `SELECT COUNT(*) FROM commissions`); n != 0 {
		t.Fatalf("crash left %d unheld commissions; planned payout %d", n, planned(t, h, inner))
	}
}

// A release before this one could credit and never screen (crash, failed
// hold write). Its replay now screens the commission, once.
func TestScreening_ReplayRepairsMissingHold(t *testing.T) {
	h, inner, e := atomicEnv(t, nil, false)
	ev := purchase("old-1", "owner@gmail.com", "")
	// The older release's credit, screening lost.
	cid, err := inner.ProcessPurchase(context.Background(), ev)
	if err != nil || cid <= 0 {
		t.Fatalf("cid=%d err=%v", cid, err)
	}

	if _, err := e.ProcessPurchase(context.Background(), ev); !errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("replay: %v, want ErrAlreadyProcessed", err)
	}
	if holdReason(t, h, cid) == "" {
		t.Fatalf("replay left the self-referral unheld; planned payout %d", planned(t, h, inner))
	}
	verdicts := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts`)

	// Idempotent: a second replay screens nothing again.
	if _, err := e.ProcessPurchase(context.Background(), ev); !errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("second replay: %v", err)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts`); n != verdicts {
		t.Fatalf("second replay re-screened: verdicts %d -> %d", verdicts, n)
	}
	if got := planned(t, h, inner); got != 0 {
		t.Fatalf("repaired commission planned into a payout of %d", got)
	}
}

// A replayed delivery of a commission this release screened is left as
// it is, even when the rules would now fire on later data.
func TestScreening_ReplayKeepsScreenedVerdict(t *testing.T) {
	h, _, e := atomicEnv(t, nil, false)
	ev := purchase("ok-1", "buyer@else.dev", "")
	cid, err := e.ProcessPurchase(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE affiliates SET email = 'buyer@else.dev' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ProcessPurchase(context.Background(), ev); !errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("replay: %v", err)
	}
	if r := holdReason(t, h, cid); r != "" {
		t.Fatalf("replay re-screened a screened commission: %s", r)
	}
}

// A replay whose repair cannot write the hold errors (the provider
// retries) instead of reporting the replay as done.
func TestScreening_ReplayRepairFailureIsReported(t *testing.T) {
	h, inner, e := atomicEnv(t, nil, false)
	ev := purchase("old-2", "owner@gmail.com", "")
	if _, err := inner.ProcessPurchase(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	restore := failHolds(t, h)
	_, err := e.ProcessPurchase(context.Background(), ev)
	if err == nil || errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("replay with a failing hold write: %v, want a retryable error", err)
	}
	restore()
	if _, err := e.ProcessPurchase(context.Background(), ev); !errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("replay after the fault: %v", err)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commission_holds`); n != 1 {
		t.Fatalf("holds = %d, want 1", n)
	}
}

// Shadow mode records the verdict with the credit and holds nothing, on
// the first delivery and on a replay.
func TestScreening_DryRunInTransaction(t *testing.T) {
	h, inner, e := atomicEnv(t, nil, true)
	ev := purchase("dry-1", "owner@gmail.com", "")
	cid, err := e.ProcessPurchase(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ProcessPurchase(context.Background(), ev); !errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("replay: %v", err)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts WHERE dry_run = TRUE`); n != 1 {
		t.Fatalf("dry-run verdicts = %d, want 1", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commission_holds`); n != 0 {
		t.Fatalf("dry-run held %d", n)
	}
	if got := planned(t, h, inner); got == 0 {
		t.Fatalf("dry-run commission %d not payable", cid)
	}
}
