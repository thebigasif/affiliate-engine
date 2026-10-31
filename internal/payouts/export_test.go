package payouts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// Manual-rail export claim (dual-engine). The PayPal/Wise file must list
// exactly what Approve would pay, only for its own rail, and only once.

func exportedIDs(res payouts.ExportResult) []int64 {
	var out []int64
	for _, p := range res.Payouts {
		out = append(out, p.PayoutID)
	}
	return out
}

func payoutOf(t *testing.T, h *db.Handles, aff int64) int64 {
	t.Helper()
	var id int64
	if err := h.DB.QueryRow(`SELECT id FROM payouts WHERE affiliate_id = ? AND status = 'requested'`, aff).Scan(&id); err != nil {
		t.Fatalf("payout for affiliate %d: %v", aff, err)
	}
	return id
}

func noRail(t *testing.T) func(int64, int64) error {
	return func(id, _ int64) error {
		t.Errorf("rail executed for exported payout %d (would pay twice)", id)
		return nil
	}
}

// X1: method filter + claim once. A paypal payout is only in the PayPal
// file, a wise payout only in the Wise file, and neither is listed again.
func TestExport_MethodFilterAndClaimOnce(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	matureOne(t, e, h, "x1-a", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 0, "paypal"); err != nil || n != 1 {
		t.Fatalf("paypal batch: n=%d err=%v", n, err)
	}
	matureOne(t, e, h, "x1-b", 2, 20000)
	if _, n, err := r.CreateBatch(ctx, 0, "wise"); err != nil || n != 1 {
		t.Fatalf("wise batch: n=%d err=%v", n, err)
	}
	pp, wp := payoutOf(t, h, 1), payoutOf(t, h, 2)

	res, err := r.ClaimForExport(ctx, "paypal")
	if err != nil {
		t.Fatal(err)
	}
	if ids := exportedIDs(res); len(ids) != 1 || ids[0] != pp || res.Payouts[0].Email != "a@t.dev" || res.Payouts[0].Amount != 1000 {
		t.Fatalf("paypal file=%+v want payout %d only", res.Payouts, pp)
	}
	res, err = r.ClaimForExport(ctx, "wise")
	if err != nil {
		t.Fatal(err)
	}
	if ids := exportedIDs(res); len(ids) != 1 || ids[0] != wp {
		t.Fatalf("wise file=%v want payout %d only", ids, wp)
	}
	for _, m := range []string{"paypal", "wise"} {
		res, err := r.ClaimForExport(ctx, m)
		if err != nil || len(res.Payouts) != 0 {
			t.Fatalf("re-export %s listed %v (err %v)", m, exportedIDs(res), err)
		}
	}
	// The claim is write-once: no path can clear it to re-list the payout.
	if _, err := h.DB.Exec(`UPDATE payouts SET exported_at = NULL WHERE id = ?`, pp); err == nil {
		t.Fatal("export claim cleared (guard missing)")
	}
	// Approve settles an exported payout without running a rail.
	if err := r.Approve(ctx, pp, noRail(t)); err != nil {
		t.Fatal(err)
	}
	var st string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pp).Scan(&st)
	if st != "paid" {
		t.Fatalf("exported payout status=%s want paid", st)
	}
}

// X2: the export runs Approve's gate. Blocked payouts stay out of the file
// and unclaimed; once cleared they export.
func TestExport_AppliesGate(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name, block, clear string
	}{
		{
			"hold",
			`INSERT INTO commission_holds (commission_id, reason) SELECT commission_id, 'flag' FROM payout_lines WHERE payout_id = ?`,
			`UPDATE commission_holds SET resolved_at = '` + now + `' WHERE commission_id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)`,
		},
		{
			"taxdoc_pending",
			`INSERT INTO tax_documents (affiliate_id, doc_type, status) SELECT affiliate_id, 'w9', 'pending' FROM payouts WHERE id = ?`,
			`UPDATE tax_documents SET status = 'received' WHERE affiliate_id = (SELECT affiliate_id FROM payouts WHERE id = ?)`,
		},
		{
			"taxdoc_rejected",
			`INSERT INTO tax_documents (affiliate_id, doc_type, status) SELECT affiliate_id, 'w8ben', 'rejected' FROM payouts WHERE id = ?`,
			`UPDATE tax_documents SET status = 'validated' WHERE affiliate_id = (SELECT affiliate_id FROM payouts WHERE id = ?)`,
		},
		{
			"commission_left_approved",
			`UPDATE commissions SET status = 'cancelled' WHERE id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)`,
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r, e := newEnv(t)
			ctx := context.Background()
			matureOne(t, e, h, "x2-"+tc.name, 1, 10000)
			if _, n, err := r.CreateBatch(ctx, 0, "paypal"); err != nil || n != 1 {
				t.Fatalf("batch: n=%d err=%v", n, err)
			}
			pid := payoutOf(t, h, 1)
			if _, err := h.DB.Exec(tc.block, pid); err != nil {
				t.Fatal(err)
			}
			res, err := r.ClaimForExport(ctx, "paypal")
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Payouts) != 0 || res.Skipped != 1 {
				t.Fatalf("blocked payout in file: %v skipped=%d", exportedIDs(res), res.Skipped)
			}
			var claimed bool
			h.DB.QueryRow(`SELECT exported_at IS NOT NULL FROM payouts WHERE id = ?`, pid).Scan(&claimed)
			if claimed {
				t.Fatal("blocked payout claimed")
			}
			if tc.clear == "" {
				return
			}
			if _, err := h.DB.Exec(tc.clear, pid); err != nil {
				t.Fatal(err)
			}
			res, err = r.ClaimForExport(ctx, "paypal")
			if err != nil || len(res.Payouts) != 1 {
				t.Fatalf("cleared payout not exported: %v err=%v", exportedIDs(res), err)
			}
		})
	}
}

// X3: an exported payout the gate blocks at Approve is flagged, never
// restaged: its lines stay, it stays 'requested' and claimed, and the
// next batch does not pay its commissions again. Once the cause clears,
// Approve settles it and the flag is cleared.
func TestExport_BlockedAtApproveNeedsAttention(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	matureOne(t, e, h, "x3-a", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 0, "paypal"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	pid := payoutOf(t, h, 1)
	if res, err := r.ClaimForExport(ctx, "paypal"); err != nil || len(res.Payouts) != 1 {
		t.Fatalf("export: %v err=%v", exportedIDs(res), err)
	}
	// The operator pays the file; then a tax doc lands pending.
	setTaxDoc(t, h, 1, "w9", "pending")
	err := r.Approve(ctx, pid, noRail(t))
	if !errors.Is(err, payouts.ErrNeedsAttention) {
		t.Fatalf("approve blocked exported payout: %v (want ErrNeedsAttention)", err)
	}
	var st, attention string
	var lines int
	h.DB.QueryRow(`SELECT status, COALESCE(attention, '') FROM payouts WHERE id = ?`, pid).Scan(&st, &attention)
	h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines WHERE payout_id = ?`, pid).Scan(&lines)
	if st != "requested" || attention == "" || lines != 1 {
		t.Fatalf("status=%s attention=%q lines=%d; want requested, flagged, lines kept", st, attention, lines)
	}
	setTaxDoc(t, h, 1, "w9", "validated")
	if _, n, _ := r.CreateBatch(ctx, 0, "paypal"); n != 0 {
		t.Fatalf("flagged payout's commissions re-batched into %d new payout(s)", n)
	}
	if err := r.Approve(ctx, pid, noRail(t)); err != nil {
		t.Fatal(err)
	}
	h.DB.QueryRow(`SELECT status, COALESCE(attention, '') FROM payouts WHERE id = ?`, pid).Scan(&st, &attention)
	if st != "paid" || attention != "" {
		t.Fatalf("after clear: status=%s attention=%q", st, attention)
	}
}

// X4: the restage path. A refund after export would make a normal payout
// restage (lines released, commissions re-batched). An exported payout may
// already be paid, so it is flagged instead and nothing is re-batched.
func TestExport_RefundAfterExportNeverRestages(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	wireLedger(t, h)
	c1 := creditMature(t, e, h, mkEvent("x4-1", 1, 10000))
	matureOne(t, e, h, "x4-2", 1, 10000)
	if _, n, err := r.CreateBatch(ctx, 0, "wise"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	pid := payoutOf(t, h, 1)
	if res, err := r.ClaimForExport(ctx, "wise"); err != nil || len(res.Payouts) != 1 {
		t.Fatalf("export: %v err=%v", exportedIDs(res), err)
	}
	if err := e.ProcessRefund(ctx, refundOf("x4-ref"), c1, 10000, 10000); err != nil {
		t.Fatal(err)
	}
	err := r.Approve(ctx, pid, noRail(t))
	if !errors.Is(err, payouts.ErrNeedsAttention) || errors.Is(err, payouts.ErrRestaged) {
		t.Fatalf("approve after refund: %v (want ErrNeedsAttention, not restaged)", err)
	}
	var lines int
	h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines WHERE payout_id = ?`, pid).Scan(&lines)
	if lines != 2 {
		t.Fatalf("exported payout lines=%d want 2 (never released)", lines)
	}
	if _, n, _ := r.CreateBatch(ctx, 0, "wise"); n != 0 {
		t.Fatalf("surviving commission re-batched into %d payout(s): paid twice", n)
	}
}
