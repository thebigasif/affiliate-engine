package commissions_test

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
)

// Atomic money writes: the journal and the domain rows it pays for commit
// in ONE transaction or not at all. Each test injects a failure into the
// domain half (a trigger that aborts the write) and checks that no money
// moved; then removes the trigger and checks the retry moves it once.
//
// Before the fix every path posted the journal in its own transaction
// and then wrote the domain rows, relying on fire-and-forget reversing
// journals (ignored errors) or a later retry to repair the gap.

// injectFailure installs a BEFORE <op> trigger on table that aborts when
// cond (a boolean SQL expression over NEW) holds. It returns the remover.
func injectFailure(t *testing.T, h *db.Handles, table, op, cond string) func() {
	t.Helper()
	name := "inject_" + table + "_" + op
	if dbtest.IsPostgres() {
		if _, err := h.DB.Exec(`CREATE OR REPLACE FUNCTION ae_inject() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`CREATE TRIGGER ` + name + ` BEFORE ` + op + ` ON ` + table +
			` FOR EACH ROW WHEN (` + cond + `) EXECUTE FUNCTION ae_inject()`); err != nil {
			t.Fatal(err)
		}
		return func() {
			if _, err := h.DB.Exec(`DROP TRIGGER ` + name + ` ON ` + table); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := h.DB.Exec(`CREATE TRIGGER ` + name + ` BEFORE ` + op + ` ON ` + table +
		` WHEN ` + cond + ` BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := h.DB.Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
}

// revertFails is the real poster with every reversing journal failing:
// the old purchase path's only repair for a failed commission insert was
// a purchase_revert post whose error it ignored.
type revertFails struct{ *ledger.Poster }

func (r revertFails) Post(ctx context.Context, idem, source, desc string, eff time.Time, lines []ledger.Line) (int64, error) {
	if source == "purchase_revert" {
		return 0, errors.New("injected: reversing journal failed")
	}
	return r.Poster.Post(ctx, idem, source, desc, eff, lines)
}

func journalsLike(t *testing.T, e *env, pattern string) int {
	t.Helper()
	var n int
	if err := e.h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE idem_key LIKE ?`, pattern).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A commission insert that fails after the credit journal posted used to
// leave the journal behind; if the ignored reversing post also failed,
// HOLD carried a liability with no commission behind it.
func TestAtomic_PurchaseInsertFailureLeavesNoJournal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	eng := commissions.NewEngine(e.h.DB, revertFails{e.p})
	drop := injectFailure(t, e.h, "commissions", "INSERT", "NEW.gross_minor = 7777")

	ev := purchase("atom-p", 7777)
	if _, err := eng.ProcessPurchase(ctx, ev); err == nil {
		t.Fatal("injected insert failure not reported")
	}
	if n := journalsLike(t, e, "%atom-p%"); n != 0 {
		t.Errorf("%d journal(s) left behind by a purchase whose commission never landed", n)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Errorf("HOLD=%d after a failed purchase, want 0", hold)
	}

	drop()
	cid, err := eng.ProcessPurchase(ctx, ev)
	if err != nil || cid == 0 {
		t.Fatalf("retry: cid=%d err=%v", cid, err)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != -777 {
		t.Fatalf("HOLD=%d after retry, want -777 (one live commission)", hold)
	}
	if s, err := e.p.GlobalIntegrity(ctx); err != nil || s != 0 {
		t.Fatalf("integrity=%d err=%v", s, err)
	}
}

// Data from a pre-atomic release can hold a credit journal with no
// commission row (the old post-then-insert gap). The next delivery adopts
// it when the amount still matches, and otherwise retires it (its revert
// spends the key pair) and credits under the next pair — in the same
// transaction, so the event finishes in one call.
func TestAtomic_LegacyDanglingCredit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	post := func(key string, amount int64) {
		t.Helper()
		if _, err := e.p.Post(ctx, key, "conversion", "", time.Now().UTC(), []ledger.Line{
			{Account: "EXPENSE:COMMISSION:USD", Amount: amount},
			{Account: "HOLD:USD", Amount: -amount},
		}); err != nil {
			t.Fatal(err)
		}
	}

	post("commission:stripe:legacy-same", 1000)
	cid, err := e.e.ProcessPurchase(ctx, purchase("legacy-same", 10000))
	if err != nil || cid == 0 {
		t.Fatalf("adopt: cid=%d err=%v", cid, err)
	}
	if n := journalsLike(t, e, "%legacy-same%"); n != 1 {
		t.Fatalf("adopt: journals=%d want 1 (the dangling credit adopted)", n)
	}

	post("commission:stripe:legacy-diff", 500)
	cid, err = e.e.ProcessPurchase(ctx, purchase("legacy-diff", 10000))
	if err != nil || cid == 0 {
		t.Fatalf("retire: cid=%d err=%v", cid, err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("retire: amount=%d want 1000", got)
	}
	for _, key := range []string{"commission:stripe:legacy-diff", "purchase-revert:stripe:legacy-diff", "commission:stripe:legacy-diff#1"} {
		if n := journalsLike(t, e, key); n != 1 {
			t.Fatalf("retire: journal %s count=%d want 1", key, n)
		}
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != -2000 {
		t.Fatalf("HOLD=%d want -2000 (two live commissions; the retired 500 netted out)", hold)
	}
	if s, err := e.p.GlobalIntegrity(ctx); err != nil || s != 0 {
		t.Fatalf("integrity=%d err=%v", s, err)
	}
}

// A clawback row that fails after the parent moved and the claw journal
// posted used to leave both behind: a cancelled parent and a claw with no
// row recording it.
func TestAtomic_RefundRowFailureMovesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("atom-r", 10000))
	if err != nil {
		t.Fatal(err)
	}
	drop := injectFailure(t, e.h, "commissions", "INSERT", "NEW.amount < 0")

	ref := refundEvent("atom-r-ref")
	if err := e.e.ProcessRefund(ctx, ref, cid, 10000, 10000); err == nil {
		t.Fatal("injected clawback-row failure not reported")
	}
	if s := parentStatus(t, e, cid); s != "pending" {
		t.Errorf("parent status=%s after a failed refund, want pending", s)
	}
	if n := journalsLike(t, e, "clawback:%"); n != 0 {
		t.Errorf("%d claw journal(s) posted by a refund whose clawback row never landed", n)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != -1000 {
		t.Errorf("HOLD=%d after a failed refund, want -1000 (nothing clawed)", hold)
	}

	drop()
	if err := e.e.ProcessRefund(ctx, ref, cid, 10000, 10000); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s := parentStatus(t, e, cid); s != "cancelled" {
		t.Fatalf("parent status=%s after retry, want cancelled", s)
	}
	if n, sum := clawRows(t, e, cid); n != 1 || sum != -1000 {
		t.Fatalf("clawback rows=%d sum=%d, want 1/-1000", n, sum)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != 0 {
		t.Fatalf("HOLD=%d after retry, want 0", hold)
	}
}

// A maturation whose status write fails used to have already moved the
// money HOLD -> payable for a commission still 'pending'.
func TestAtomic_MatureStatusFailureMovesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid := dueCommission(t, e, "atom-m")
	holdBefore, payBefore := balances(t, e)
	drop := injectFailure(t, e.h, "commissions", "UPDATE", "NEW.status = 'approved'")

	if _, err := e.e.Mature(ctx); err == nil {
		t.Fatal("injected status failure not reported")
	}
	if s := statusOf(t, e, cid); s != "pending" {
		t.Errorf("status=%s, want pending", s)
	}
	if h, p := balances(t, e); h != holdBefore || p != payBefore {
		t.Errorf("money moved for a commission that did not mature: HOLD %d->%d payable %d->%d",
			holdBefore, h, payBefore, p)
	}

	drop()
	if n, err := e.e.Mature(ctx); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if h, p := balances(t, e); h != holdBefore+1000 || p != payBefore-1000 {
		t.Fatalf("after retry HOLD=%d payable=%d (from %d/%d); want 1000 moved once", h, p, holdBefore, payBefore)
	}
}

// An uphold whose cancel fails used to have already clawed the money out
// of HOLD for a commission still 'pending' with its hold open.
func TestAtomic_UpholdCancelFailureMovesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("atom-u", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'test')`, cid); err != nil {
		t.Fatal(err)
	}
	holdBefore, _ := balances(t, e)
	drop := injectFailure(t, e.h, "commission_holds", "UPDATE", "NEW.resolution = 'upheld'")

	if err := fraud.Uphold(ctx, e.h.DB, e.p, cid, "fraud"); err == nil {
		t.Fatal("injected cancel failure not reported")
	}
	if s := statusOf(t, e, cid); s != "pending" {
		t.Errorf("status=%s, want pending", s)
	}
	if h, _ := balances(t, e); h != holdBefore {
		t.Errorf("HOLD %d -> %d: clawed for a commission that was not cancelled", holdBefore, h)
	}

	drop()
	if err := fraud.Uphold(ctx, e.h.DB, e.p, cid, "fraud"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s := statusOf(t, e, cid); s != "cancelled" {
		t.Fatalf("status=%s after retry, want cancelled", s)
	}
	if h, _ := balances(t, e); h != holdBefore+1000 {
		t.Fatalf("HOLD=%d after retry, want %d", h, holdBefore+1000)
	}
}
