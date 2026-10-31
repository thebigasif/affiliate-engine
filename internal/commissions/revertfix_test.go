package commissions_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// failOnce forwards to the real poster but fails the first post whose
// source matches.
type failOnce struct {
	*ledger.Poster
	src   string
	armed bool
}

func (f *failOnce) Post(ctx context.Context, idemKey, source, desc string, eff time.Time, lines []ledger.Line) (int64, error) {
	if f.armed && source == f.src {
		f.armed = false
		return 0, errors.New("injected: transient journal failure")
	}
	return f.Poster.Post(ctx, idemKey, source, desc, eff, lines)
}

func dueCommission(t *testing.T, e *env, id string) int64 {
	t.Helper()
	cid, err := e.e.ProcessPurchase(context.Background(), purchase(id, 10000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`UPDATE commissions SET matures_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), cid); err != nil {
		t.Fatal(err)
	}
	return cid
}

func balances(t *testing.T, e *env) (hold, payable int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	if hold, err = e.p.AccountBalance(ctx, "HOLD:USD"); err != nil {
		t.Fatal(err)
	}
	if payable, err = e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD"); err != nil {
		t.Fatal(err)
	}
	return hold, payable
}

func statusOf(t *testing.T, e *env, cid int64) string {
	t.Helper()
	var s string
	if err := e.h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A failed maturation post used to leave the commission 'approved' with
// its money still in HOLD (the approved->pending revert is an illegal
// transition). Now the post comes first: a failure changes nothing and
// the next run matures it.
func TestMature_FailedPostChangesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid := dueCommission(t, e, "mat-fail")
	holdBefore, payBefore := balances(t, e)

	bad := commissions.NewEngine(e.h.DB, &failOnce{Poster: e.p, src: "maturation", armed: true})
	if _, err := bad.Mature(ctx); err == nil {
		t.Fatal("injected failure not reported")
	}
	if s := statusOf(t, e, cid); s != "pending" {
		t.Fatalf("status after failed post = %s, want pending", s)
	}
	if h, p := balances(t, e); h != holdBefore || p != payBefore {
		t.Fatalf("money moved on a failed post: HOLD %d->%d payable %d->%d", holdBefore, h, payBefore, p)
	}

	if n, err := e.e.Mature(ctx); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if s := statusOf(t, e, cid); s != "approved" {
		t.Fatalf("status after retry = %s", s)
	}
	if h, p := balances(t, e); h != holdBefore+1000 || p != payBefore-1000 {
		t.Fatalf("after maturation HOLD=%d payable=%d (from %d/%d); want 1000 moved once", h, p, holdBefore, payBefore)
	}
}

// Crash between the journal and the status CAS: the next run dedupes the
// journal and finishes the CAS, moving the money exactly once.
func TestMature_JournalLandedStatusDidNot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid := dueCommission(t, e, "mat-crash")
	holdBefore, payBefore := balances(t, e)
	if _, err := e.p.Post(ctx, fmt.Sprintf("mature:%d", cid), "maturation", "", time.Now().UTC(), []ledger.Line{
		{Account: "HOLD:USD", Amount: 1000},
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: -1000},
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.e.Mature(ctx); err != nil || n != 1 {
		t.Fatalf("mature: n=%d err=%v", n, err)
	}
	if s := statusOf(t, e, cid); s != "approved" {
		t.Fatalf("status = %s", s)
	}
	if h, p := balances(t, e); h != holdBefore+1000 || p != payBefore-1000 {
		t.Fatalf("HOLD=%d payable=%d (from %d/%d); want 1000 moved exactly once", h, p, holdBefore, payBefore)
	}
}

// A failed uphold clawback used to leave the commission 'cancelled' with
// its hold resolved and the money unmoved. Now the claw posts first: a
// failure changes nothing, and the retry claws and cancels.
func TestUphold_FailedClawChangesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("uphold-fail", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'test')`, cid); err != nil {
		t.Fatal(err)
	}
	holdBefore, _ := balances(t, e)

	if err := fraud.Uphold(ctx, e.h.DB, &failOnce{Poster: e.p, src: "fraud_clawback", armed: true}, cid, "fraud"); err == nil {
		t.Fatal("injected failure not reported")
	}
	var open int
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds WHERE commission_id = ? AND resolved_at IS NULL`, cid).Scan(&open)
	if s := statusOf(t, e, cid); s != "pending" || open != 1 {
		t.Fatalf("after failed claw: status=%s open holds=%d; want pending with the hold open", s, open)
	}
	if h, _ := balances(t, e); h != holdBefore {
		t.Fatalf("HOLD moved on a failed claw: %d -> %d", holdBefore, h)
	}

	if err := fraud.Uphold(ctx, e.h.DB, e.p, cid, "fraud"); err != nil {
		t.Fatal(err)
	}
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds WHERE commission_id = ? AND resolved_at IS NULL`, cid).Scan(&open)
	if s := statusOf(t, e, cid); s != "cancelled" || open != 0 {
		t.Fatalf("after retry: status=%s open holds=%d", s, open)
	}
	if h, _ := balances(t, e); h != holdBefore+1000 {
		t.Fatalf("HOLD=%d want %d (the 1000 clawed back out of it)", h, holdBefore+1000)
	}
	// A second uphold is an idempotent reject and moves nothing.
	if err := fraud.Uphold(ctx, e.h.DB, e.p, cid, "fraud"); !errors.Is(err, fraud.ErrTerminalState) {
		t.Fatalf("second uphold: %v", err)
	}
}
