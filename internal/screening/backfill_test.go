package screening_test

// Backfill screens the commissions older releases credited and never
// screened (screened_at NULL), with the same eligibility and the same
// ScreenFunc as the replay repair, without waiting for the provider to
// re-deliver each event.

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

type backfillFixture struct {
	selfRef, clean, held, paid, released int64
}

// oldStyle credits through the inner engine (no screening, screened_at
// NULL), as a release before screened_at did, then arranges each case.
func oldStyle(t *testing.T, h *db.Handles, credit func(id, email string) int64) backfillFixture {
	t.Helper()
	f := backfillFixture{
		selfRef:  credit("bf-self", "Owner+promo@gmail.com"),
		clean:    credit("bf-clean", "buyer@else.dev"),
		held:     credit("bf-held", "owner@gmail.com"),
		paid:     credit("bf-paid", "owner@gmail.com"),
		released: credit("bf-released", "owner@gmail.com"),
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'manual')`, []any{f.held}},
		{`INSERT INTO commission_holds (commission_id, reason, resolved_at, resolution)
		  VALUES (?, 'fraud review: self_referral', CURRENT_TIMESTAMP, 'released')`, []any{f.released}},
		{`UPDATE commissions SET status = 'approved' WHERE id = ?`, []any{f.paid}},
		{`UPDATE commissions SET status = 'paid' WHERE id = ?`, []any{f.paid}},
	} {
		if _, err := h.DB.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commissions WHERE screened_at IS NULL`); n != 5 {
		t.Fatalf("fixture: %d unscreened commissions, want 5", n)
	}
	return f
}

func backfillEnv(t *testing.T, dryRun bool) (*db.Handles, *screening.Engine, backfillFixture) {
	t.Helper()
	h, inner, e := atomicEnv(t, nil, dryRun)
	f := oldStyle(t, h, func(id, email string) int64 {
		cid, err := inner.ProcessPurchase(context.Background(), purchase(id, email, ""))
		if err != nil || cid <= 0 {
			t.Fatalf("credit %s: cid=%d err=%v", id, cid, err)
		}
		return cid
	})
	return h, e, f
}

func TestBackfill_HoldsOnlyTheUnscreenedSelfReferral(t *testing.T) {
	h, e, f := backfillEnv(t, false)
	ctx := context.Background()
	rep, err := e.Backfill(ctx, h.DB, screening.BackfillOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 2 || rep.Screened != 2 || rep.Held != 1 || rep.Errors != 0 {
		t.Fatalf("report %+v, want scanned 2, screened 2, held 1", rep)
	}
	if rep.HeldBy["self_referral"] != 1 {
		t.Fatalf("held by %v, want self_referral 1", rep.HeldBy)
	}
	if r := holdReason(t, h, f.selfRef); r != "fraud review: self_referral" {
		t.Fatalf("self-referral hold = %q", r)
	}
	if r := holdReason(t, h, f.clean); r != "" {
		t.Fatalf("clean commission held: %q", r)
	}
	if r := holdReason(t, h, f.held); r != "manual" {
		t.Fatalf("existing hold changed: %q", r)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commission_holds WHERE commission_id IN (?, ?)`, f.paid, f.released); n != 1 {
		t.Fatalf("paid / released holds = %d, want only the released one", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commission_holds WHERE commission_id = ? AND resolved_at IS NULL`, f.released); n != 0 {
		t.Fatal("a reviewer's release was re-opened")
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commissions WHERE screened_at IS NOT NULL`); n != 2 {
		t.Fatalf("screened_at set on %d commissions, want 2", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commissions WHERE screened_at IS NOT NULL AND id IN (?, ?)`, f.selfRef, f.clean); n != 2 {
		t.Fatal("screened_at not set on the screened commissions")
	}

	// Idempotent: a second run finds nothing and writes nothing.
	verdicts := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts`)
	holds := count(t, h, `SELECT COUNT(*) FROM commission_holds`)
	rep, err = e.Backfill(ctx, h.DB, screening.BackfillOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 0 || rep.Screened != 0 || rep.Held != 0 {
		t.Fatalf("rerun %+v, want nothing", rep)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts`); n != verdicts {
		t.Fatalf("rerun wrote verdicts: %d -> %d", verdicts, n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commission_holds`); n != holds {
		t.Fatalf("rerun wrote holds: %d -> %d", holds, n)
	}
}

func TestBackfill_DryRunWritesNothing(t *testing.T) {
	h, e, f := backfillEnv(t, false)
	snapshot := func() [3]int {
		return [3]int{
			count(t, h, `SELECT COUNT(*) FROM commission_holds`),
			count(t, h, `SELECT COUNT(*) FROM fraud_verdicts`),
			count(t, h, `SELECT COUNT(*) FROM commissions WHERE screened_at IS NOT NULL`),
		}
	}
	before := snapshot()
	rep, err := e.Backfill(context.Background(), h.DB, screening.BackfillOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Scanned != 2 || rep.Screened != 2 || rep.Held != 1 || rep.HeldBy["self_referral"] != 1 {
		t.Fatalf("dry-run report %+v, want the self-referral as would-hold", rep)
	}
	if got := snapshot(); got != before {
		t.Fatalf("dry-run wrote: holds/verdicts/screened %v -> %v", before, got)
	}
	if r := holdReason(t, h, f.selfRef); r != "" {
		t.Fatalf("dry-run held the self-referral: %q", r)
	}
	// The real run after a dry run still does the work.
	rep, err = e.Backfill(context.Background(), h.DB, screening.BackfillOptions{})
	if err != nil || rep.Held != 1 {
		t.Fatalf("real run after dry run: %+v %v", rep, err)
	}
}

// AE_FRAUD_DRYRUN (the engine's shadow mode): verdicts are recorded and
// nothing is held, exactly as a fresh credit in shadow mode.
func TestBackfill_ShadowModeHoldsNothing(t *testing.T) {
	h, e, _ := backfillEnv(t, true)
	rep, err := e.Backfill(context.Background(), h.DB, screening.BackfillOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Screened != 2 || rep.Held != 0 {
		t.Fatalf("shadow report %+v, want 2 screened, 0 held", rep)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts WHERE dry_run = TRUE AND rule = 'self_referral'`); n != 1 {
		t.Fatalf("shadow verdicts = %d, want 1", n)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commission_holds`); n != 2 {
		t.Fatalf("holds = %d, want only the fixture's 2", n)
	}
}

// -limit screens at most N per run; the next run picks up the rest.
func TestBackfill_LimitResumes(t *testing.T) {
	h, e, f := backfillEnv(t, false)
	ctx := context.Background()
	rep, err := e.Backfill(ctx, h.DB, screening.BackfillOptions{Limit: 1})
	if err != nil || rep.Scanned != 1 || rep.Screened != 1 {
		t.Fatalf("limit 1: %+v %v", rep, err)
	}
	rep, err = e.Backfill(ctx, h.DB, screening.BackfillOptions{Limit: 1})
	if err != nil || rep.Scanned != 1 || rep.Screened != 1 {
		t.Fatalf("second limit 1: %+v %v", rep, err)
	}
	if holdReason(t, h, f.selfRef) == "" {
		t.Fatal("self-referral unheld after two limited runs")
	}
	if rep, _ := e.Backfill(ctx, h.DB, screening.BackfillOptions{}); rep.Scanned != 0 {
		t.Fatalf("third run %+v, want nothing left", rep)
	}
}

// A hold write that fails is counted, leaves the commission unscreened,
// and the next run repairs it.
func TestBackfill_FailedHoldIsRetried(t *testing.T) {
	h, e, f := backfillEnv(t, false)
	ctx := context.Background()
	restore := failHolds(t, h)
	rep, err := e.Backfill(ctx, h.DB, screening.BackfillOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors != 1 || rep.Screened != 1 {
		t.Fatalf("report %+v, want 1 error (the self-referral) and 1 screened", rep)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM commissions WHERE id = ? AND screened_at IS NULL`, f.selfRef); n != 1 {
		t.Fatal("a failed screening marked the commission screened")
	}
	restore()
	rep, err = e.Backfill(ctx, h.DB, screening.BackfillOptions{})
	if err != nil || rep.Held != 1 || rep.Errors != 0 {
		t.Fatalf("retry %+v %v", rep, err)
	}
}

// -since leaves older commissions for a later pass.
func TestBackfill_Since(t *testing.T) {
	h, e, f := backfillEnv(t, false)
	if _, err := h.DB.Exec(`UPDATE commissions SET created_at = ? WHERE id = ?`, "2020-01-01T00:00:00Z", f.selfRef); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Truncate(24 * time.Hour)
	rep, err := e.Backfill(context.Background(), h.DB, screening.BackfillOptions{Since: since})
	if err != nil || rep.Scanned != 1 || rep.Held != 0 {
		t.Fatalf("since today: %+v %v, want only the clean commission", rep, err)
	}
	if holdReason(t, h, f.selfRef) != "" {
		t.Fatal("-since screened an older commission")
	}
}
