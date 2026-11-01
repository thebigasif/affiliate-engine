package mlm_test

// Refunds in MLM totals: refunded or reversed commissions never earn
// overrides, before or after the period run.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

var refundSeq int

// seedSource inserts a commission of amount for affiliate 3 with status and
// returns its id.
func seedSource(t *testing.T, h *db.Handles, amount int64, status string) int64 {
	t.Helper()
	refundSeq++
	key := fmt.Sprintf("mlm-ref-%d", refundSeq)
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'conversion', ?)`,
		key, nowRFC()); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := h.DB.QueryRow(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
		VALUES (3, (SELECT id FROM journal_entries WHERE idem_key = ?), ?, '{}', ?, ?) RETURNING id`,
		key, amount, status, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedClawback records a refund of claw against parent, the way the
// commissions engine does (a negative, cancelled child row).
func seedClawback(t *testing.T, h *db.Handles, parent, claw int64) {
	t.Helper()
	refundSeq++
	key := fmt.Sprintf("mlm-claw-%d", refundSeq)
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES (?, 'clawback', ?)`,
		key, nowRFC()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, parent_commission_id, created_at)
		VALUES (3, (SELECT id FROM journal_entries WHERE idem_key = ?), ?, '{"origin":"clawback"}', 'cancelled', ?, ?)`,
		key, -claw, parent, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func runPeriod(t *testing.T, h *db.Handles, run func(ctx context.Context, start, end time.Time) (int64, error)) int64 {
	t.Helper()
	now := time.Now().UTC()
	if _, err := run(context.Background(), now.Add(-24*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var periodID int64
	if err := h.DB.QueryRow(`SELECT id FROM mlm_payout_periods ORDER BY id DESC LIMIT 1`).Scan(&periodID); err != nil {
		t.Fatal(err)
	}
	return periodID
}

// MR1: the period run splits a partly refunded commission on what is left
// of it, and skips refunded-in-full, cancelled and clawed-back ones.
func TestMLM_RunPeriodNetsRefunds(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	partial := seedSource(t, h, 10000, "approved")
	seedClawback(t, h, partial, 4000) // 6000 left
	exhausted := seedSource(t, h, 10000, "paid")
	seedClawback(t, h, exhausted, 10000) // nothing left, status not moved yet
	seedSource(t, h, 10000, "cancelled") // refunded in full before maturing
	seedSource(t, h, 10000, "clawback")  // refunded in full after payout

	periodID := runPeriod(t, h, e.RunPeriod)
	var sources int
	if err := h.DB.QueryRow(`SELECT COUNT(DISTINCT source_commission_id) FROM mlm_period_lines`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources != 1 {
		t.Fatalf("overrides from %d source commissions, want only the partly refunded one", sources)
	}
	ctx := context.Background()
	tot2, err := e.PeriodTotal(ctx, periodID, 2)
	if err != nil {
		t.Fatal(err)
	}
	tot1, err := e.PeriodTotal(ctx, periodID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if tot2["USD"] != 1800 || tot1["USD"] != 600 {
		t.Fatalf("overrides L1=%v L2=%v, want 30%% and 10%% of 6000 (1800, 600)", tot2, tot1)
	}
}

// MR2: a refund after the run lowers the override total; a full refund or a
// fraud uphold (cancelled) takes it to 0. The recorded lines stay.
func TestMLM_PeriodTotalNetsLaterRefunds(t *testing.T) {
	h, e := newEnv(t)
	chain(t, e)
	ctx := context.Background()
	src := seedSource(t, h, 10000, "approved")
	periodID := runPeriod(t, h, e.RunPeriod)
	total := func(aff int64) int64 {
		t.Helper()
		tot, err := e.PeriodTotal(ctx, periodID, aff)
		if err != nil {
			t.Fatal(err)
		}
		return tot["USD"]
	}
	if total(2) != 3000 || total(1) != 1000 {
		t.Fatalf("before refund: L1=%d L2=%d want 3000/1000", total(2), total(1))
	}
	seedClawback(t, h, src, 5000)
	if total(2) != 1500 || total(1) != 500 {
		t.Fatalf("after half refund: L1=%d L2=%d want 1500/500", total(2), total(1))
	}
	if _, err := h.DB.Exec(`UPDATE commissions SET status = 'cancelled' WHERE id = ?`, src); err != nil {
		t.Fatal(err)
	}
	if total(2) != 0 || total(1) != 0 {
		t.Fatalf("after cancel: L1=%d L2=%d want 0/0", total(2), total(1))
	}
	var recorded int64
	if err := h.DB.QueryRow(`SELECT SUM(commission_cents) FROM mlm_period_lines`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 4000 {
		t.Fatalf("recorded lines changed: %d want 4000", recorded)
	}
}
