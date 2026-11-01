package fraud_test

// Partner-wide holds: a verdict about the affiliate's overall behaviour
// holds the affiliate's other unpaid, not yet exported commissions in one
// review group, and releasing any hold of the group releases all of it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
)

// seedCommissionAt is seedCommission with an explicit created_at.
func seedCommissionAt(t *testing.T, h *db.Handles, aff int64, status string, at time.Time) int64 {
	t.Helper()
	key := fmt.Sprintf("aw-%d-%d", aff, seedSeq.Add(1))
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at)
		VALUES (?, 'conversion', CURRENT_TIMESTAMP)`, key); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := h.DB.QueryRow(`INSERT INTO commissions
		(affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (?, (SELECT id FROM journal_entries WHERE idem_key = ?), 100, '{}', ?, ?)
		RETURNING id`,
		aff, key, status, at.UTC().Format(time.RFC3339)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedPayout stages commission cid in a payout of aff with status, exported
// or not.
func seedPayout(t *testing.T, h *db.Handles, aff, cid int64, status string, exported bool) {
	t.Helper()
	var exportedAt any
	if exported {
		exportedAt = time.Now().UTC().Format(time.RFC3339)
	}
	var pid int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount, status, exported_at)
		VALUES (?, 100, ?, ?) RETURNING id`, aff, status, exportedAt).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO payout_lines (payout_id, commission_id) VALUES (?, ?)`, pid, cid); err != nil {
		t.Fatal(err)
	}
}

type holdRow struct {
	reason, group string
	open          bool
}

func holdRows(t *testing.T, h *db.Handles) map[int64]holdRow {
	t.Helper()
	rows, err := h.DB.Query(`SELECT commission_id, reason, hold_group, resolved_at IS NULL FROM commission_holds`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]holdRow{}
	for rows.Next() {
		var id int64
		var r holdRow
		var g sql.NullString
		if err := rows.Scan(&id, &r.reason, &g, &r.open); err != nil {
			t.Fatal(err)
		}
		r.group = g.String
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// tripVelocity seeds the hourly conversion_velocity burst for aff and
// returns the newest commission (the one under evaluation).
func tripVelocity(t *testing.T, h *db.Handles, aff int64) int64 {
	t.Helper()
	var last int64
	for i := 0; i < fraud.ConversionsPerHourLimit; i++ {
		last = seedCommissionAt(t, h, aff, "pending", time.Now().Add(-10*time.Minute))
	}
	return last
}

// AW1: a partner-wide verdict holds the affiliate's other unpaid commissions
// with the same reason and group, and leaves paid, exported, executing,
// previously released and other affiliates' commissions alone.
func TestFraud_AffiliateWideHoldsUnpaid(t *testing.T) {
	h := newDB(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'other@t.dev')`); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * 24 * time.Hour)
	approved := seedCommissionAt(t, h, 1, "approved", old)
	pending := seedCommissionAt(t, h, 1, "pending", old)
	paid := seedCommissionAt(t, h, 1, "paid", old)
	exported := seedCommissionAt(t, h, 1, "approved", old)
	seedPayout(t, h, 1, exported, "requested", true)
	executing := seedCommissionAt(t, h, 1, "approved", old)
	seedPayout(t, h, 1, executing, "processing", false)
	staged := seedCommissionAt(t, h, 1, "approved", old)
	seedPayout(t, h, 1, staged, "requested", false)
	released := seedCommissionAt(t, h, 1, "approved", old)
	if _, err := h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason, resolved_at, resolution)
		VALUES (?, 'earlier', ?, 'released')`, released, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	otherAff := seedCommissionAt(t, h, 2, "approved", old)
	target := tripVelocity(t, h, 1)

	if _, err := fraud.NewEngine(h.DB).Evaluate(context.Background(), fraud.Query{
		CommissionID: target, AffiliateID: 1, Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	holds := holdRows(t, h)
	want := holds[target]
	if !want.open || want.group == "" || want.reason != "fraud review: conversion_velocity" {
		t.Fatalf("evaluated commission hold = %+v", want)
	}
	for _, id := range []int64{approved, pending, staged} {
		got, ok := holds[id]
		if !ok || !got.open || got.group != want.group || got.reason != want.reason {
			t.Fatalf("unpaid commission %d: hold %+v (ok=%v), want open in group %q with reason %q", id, got, ok, want.group, want.reason)
		}
	}
	for _, id := range []int64{paid, exported, executing, otherAff} {
		if _, ok := holds[id]; ok {
			t.Fatalf("commission %d must not be held: %+v", id, holds[id])
		}
	}
	if holds[released].open || holds[released].reason != "earlier" {
		t.Fatalf("a released hold was re-opened: %+v", holds[released])
	}
}

// AW2: releasing any hold of a group releases the whole group; a hold outside
// the group stays open, and a second release reports no open hold.
func TestFraud_ReleaseHoldReleasesGroup(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	old := time.Now().Add(-20 * 24 * time.Hour)
	earlier := seedCommissionAt(t, h, 1, "approved", old)
	alone := seedCommissionAt(t, h, 1, "approved", old)
	if _, err := h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'manual')`, alone); err != nil {
		t.Fatal(err)
	}
	target := tripVelocity(t, h, 1)
	if _, err := fraud.NewEngine(h.DB).Evaluate(ctx, fraud.Query{CommissionID: target, AffiliateID: 1, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before := holdRows(t, h)
	if before[alone].group != "" {
		t.Fatalf("an existing ungrouped hold of another commission joined the group: %+v", before[alone])
	}
	var group []int64
	for id, r := range before {
		if r.group != "" && r.open {
			group = append(group, id)
		}
	}
	if len(group) != fraud.ConversionsPerHourLimit+1 {
		t.Fatalf("group size %d, want %d", len(group), fraud.ConversionsPerHourLimit+1)
	}

	// Release a member that is not the evaluated commission.
	also, err := fraud.ReleaseHold(ctx, h.DB, earlier, "checked, genuine")
	if err != nil {
		t.Fatal(err)
	}
	if len(also) != len(group)-1 {
		t.Fatalf("also released %d, want %d", len(also), len(group)-1)
	}
	sort.Slice(also, func(i, j int) bool { return also[i] < also[j] })
	after := holdRows(t, h)
	for _, id := range group {
		if after[id].open {
			t.Fatalf("group member %d still open after release", id)
		}
	}
	if !after[alone].open {
		t.Fatal("a hold outside the group was released")
	}
	var note, resolution string
	if err := h.DB.QueryRow(`SELECT note, resolution FROM commission_holds WHERE commission_id = ?`, target).
		Scan(&note, &resolution); err != nil {
		t.Fatal(err)
	}
	if note != "checked, genuine" || resolution != "released" {
		t.Fatalf("group member note=%q resolution=%q", note, resolution)
	}
	if _, err := fraud.ReleaseHold(ctx, h.DB, earlier, ""); !errors.Is(err, fraud.ErrNoOpenHold) {
		t.Fatalf("second release: %v, want ErrNoOpenHold", err)
	}
	// An ungrouped hold releases alone.
	also, err = fraud.ReleaseHold(ctx, h.DB, alone, "")
	if err != nil || len(also) != 0 {
		t.Fatalf("ungrouped release: also=%v err=%v", also, err)
	}
}

// AW3: a later partner-wide verdict joins the affiliate's open group rather
// than opening a second review case.
func TestFraud_AffiliateWideJoinsOpenGroup(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	e := fraud.NewEngine(h.DB)
	first := tripVelocity(t, h, 1)
	if _, err := e.Evaluate(ctx, fraud.Query{CommissionID: first, AffiliateID: 1, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	next := seedCommissionAt(t, h, 1, "pending", time.Now())
	if _, err := e.Evaluate(ctx, fraud.Query{CommissionID: next, AffiliateID: 1, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	holds := holdRows(t, h)
	if holds[next].group == "" || holds[next].group != holds[first].group {
		t.Fatalf("second verdict group %q, want the open group %q", holds[next].group, holds[first].group)
	}
}

// seedSpread inserts n sales of aff spread evenly over (now-from, now-to],
// sharing one journal entry (fast enough for a few thousand rows).
func seedSpread(t *testing.T, h *db.Handles, aff int64, n int, now time.Time, from, to time.Duration) {
	t.Helper()
	key := fmt.Sprintf("spread-%d-%d", aff, seedSeq.Add(1))
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at)
		VALUES (?, 'conversion', CURRENT_TIMESTAMP)`, key); err != nil {
		t.Fatal(err)
	}
	var jid int64
	if err := h.DB.QueryRow(`SELECT id FROM journal_entries WHERE idem_key = ?`, key).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	step := (from - to) / time.Duration(n)
	for i := 0; i < n; i++ {
		at := now.Add(-to - time.Duration(i)*step - time.Second)
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
			VALUES (?, ?, 100, '{}', 'pending', ?)`, aff, jid, at.UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
}

func velocityVerdict(t *testing.T, h *db.Handles, cid int64, now time.Time) *fraud.Verdict {
	t.Helper()
	vs, err := fraud.NewEngine(h.DB).Evaluate(context.Background(),
		fraud.Query{CommissionID: cid, AffiliateID: 1, Now: now, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range vs {
		if vs[i].Rule == "conversion_velocity" {
			if !vs[i].AffiliateWide {
				t.Fatal("conversion_velocity verdict is not partner-wide")
			}
			return &vs[i]
		}
	}
	return nil
}

// AW4: a new affiliate keeping just under the hourly limit all day trips
// the 24-hour pace check at the floor; one sale fewer does not.
func TestFraud_PaceFloor(t *testing.T) {
	for _, tc := range []struct {
		n    int
		trip bool
	}{{fraud.ConversionsPerDayFloor - 2, false}, {fraud.ConversionsPerDayFloor - 1, true}} {
		t.Run(fmt.Sprint(tc.n), func(t *testing.T) {
			h := newDB(t)
			now := time.Now()
			// Hours 2..23 ago: never 30 in any one hour. The evaluated sale
			// below makes n+1.
			seedSpread(t, h, 1, tc.n, now, 23*time.Hour, 2*time.Hour)
			cid := seedCommissionAt(t, h, 1, "pending", now.Add(-3*time.Hour))
			if got := velocityVerdict(t, h, cid, now) != nil; got != tc.trip {
				t.Fatalf("%d sales in 24h: tripped=%v want %v", tc.n+1, got, tc.trip)
			}
		})
	}
}

// AW4b: an established affiliate is judged against its own normal pace.
// 200 sales a day for the prior week sets the limit at 400; 380 in the last
// day is above the floor but below it.
func TestFraud_PaceRelativeToBaseline(t *testing.T) {
	h := newDB(t)
	now := time.Now()
	seedSpread(t, h, 1, 7*200, now, 8*24*time.Hour, 24*time.Hour+time.Minute)
	seedSpread(t, h, 1, 380, now, 23*time.Hour, 2*time.Hour)
	cid := seedCommissionAt(t, h, 1, "pending", now.Add(-3*time.Hour))
	if v := velocityVerdict(t, h, cid, now); v != nil {
		t.Fatalf("established affiliate held at its normal pace: %s", v.Reason)
	}
	seedSpread(t, h, 1, 40, now, 22*time.Hour, 3*time.Hour) // 421 sales today
	if v := velocityVerdict(t, h, cid, now); v == nil {
		t.Fatal("twice the usual pace did not trip")
	}
}

// AW4c: refund clawback rows and usage settlements are not sales.
func TestFraud_VelocityCountsSalesOnly(t *testing.T) {
	h := newDB(t)
	now := time.Now()
	parentIDs := make([]int64, 15)
	for i := range parentIDs {
		parentIDs[i] = seedCommissionAt(t, h, 1, "pending", now.Add(-10*time.Minute))
	}
	for i, p := range parentIDs {
		key := fmt.Sprintf("claw-%d-%d", i, seedSeq.Add(1))
		if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'clawback', CURRENT_TIMESTAMP)`, key); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, parent_commission_id, created_at)
			VALUES (1, (SELECT id FROM journal_entries WHERE idem_key = ?), -100, '{}', 'cancelled', ?, ?)`,
			key, p, now.Add(-5*time.Minute).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	key := fmt.Sprintf("usage-%d", seedSeq.Add(1))
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'usage_settlement', CURRENT_TIMESTAMP)`, key); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
			VALUES (1, (SELECT id FROM journal_entries WHERE idem_key = ?), 50, '{"origin":"usage"}', 'pending', ?)`,
			key, now.Add(-5*time.Minute).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if v := velocityVerdict(t, h, parentIDs[0], now); v != nil {
		t.Fatalf("15 sales plus refunds and usage rows tripped velocity: %s", v.Reason)
	}
}

// AW4d: after the owner releases a velocity hold, the same trigger does not
// hold again inside its window.
func TestFraud_VelocityNotReheldAfterRelease(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	now := time.Now()
	target := tripVelocity(t, h, 1)
	if _, err := fraud.NewEngine(h.DB).Evaluate(ctx, fraud.Query{CommissionID: target, AffiliateID: 1, Now: now}); err != nil {
		t.Fatal(err)
	}
	if !holdRows(t, h)[target].open {
		t.Fatal("burst not held")
	}
	if _, err := fraud.ReleaseHold(ctx, h.DB, target, "genuine launch"); err != nil {
		t.Fatal(err)
	}
	next := seedCommissionAt(t, h, 1, "pending", now)
	if _, err := fraud.NewEngine(h.DB).Evaluate(ctx, fraud.Query{CommissionID: next, AffiliateID: 1, Now: now}); err != nil {
		t.Fatal(err)
	}
	if r, ok := holdRows(t, h)[next]; ok {
		t.Fatalf("new sale re-held right after a release: %+v", r)
	}
}

type wideRule struct{}

func (wideRule) Name() string { return "wide_rule" }

func (wideRule) Evaluate(_ context.Context, q fraud.Query) ([]fraud.Verdict, error) {
	return []fraud.Verdict{{
		Rule: "wide_rule", Subject: fmt.Sprint(q.AffiliateID), Reason: "test",
		Action: fraud.ActionHold, AffiliateWide: true,
	}}, nil
}

// AW5: engines built from an explicit ruleset (the Shopify pipeline) keep the
// narrow scope until partner-wide holds are switched on; once on, the
// periodic sweep (no evaluated commission) holds the unpaid commissions too.
func TestFraud_AffiliateWideOptInAndSweep(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	unpaid := seedCommissionAt(t, h, 1, "approved", time.Now().Add(-48*time.Hour))
	target := seedCommissionAt(t, h, 1, "pending", time.Now())

	e := fraud.NewEngineWithRules(h.DB, wideRule{})
	if _, err := e.Evaluate(ctx, fraud.Query{CommissionID: target, AffiliateID: 1, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	holds := holdRows(t, h)
	if !holds[target].open || holds[target].group != "" {
		t.Fatalf("narrow engine: target hold %+v", holds[target])
	}
	if _, ok := holds[unpaid]; ok {
		t.Fatal("narrow engine held an unrelated commission")
	}

	h2 := newDB(t)
	unpaid2 := seedCommissionAt(t, h2, 1, "approved", time.Now().Add(-48*time.Hour))
	e2 := fraud.NewEngineWithRules(h2.DB, wideRule{})
	e2.SetAffiliateWideHolds(true)
	// Dry-run sweep: verdict recorded, nothing held.
	if _, err := e2.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: time.Now(), DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(holdRows(t, h2)) != 0 {
		t.Fatal("dry-run sweep placed holds")
	}
	if _, err := e2.Evaluate(ctx, fraud.Query{AffiliateID: 1, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := holdRows(t, h2)[unpaid2]; !r.open || r.group == "" {
		t.Fatalf("sweep partner-wide hold: %+v", r)
	}
}

// AW6: inside the credit's transaction (screening) the partner-wide hold
// commits with it.
func TestFraud_AffiliateWideInsideTx(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	unpaid := seedCommissionAt(t, h, 1, "approved", time.Now().Add(-48*time.Hour))
	target := tripVelocity(t, h, 1)
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fraud.NewEngine(h.DB).Evaluate(fraud.WithTx(ctx, tx), fraud.Query{
		CommissionID: target, AffiliateID: 1, Now: time.Now(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	holds := holdRows(t, h)
	if !holds[unpaid].open || holds[unpaid].group != holds[target].group {
		t.Fatalf("in-transaction partner-wide hold: unpaid=%+v target=%+v", holds[unpaid], holds[target])
	}
}
