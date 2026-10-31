package payouts_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

// FR-17 tax-doc gating (dual-engine). A tax_documents row in a blocking
// status ('pending' | 'rejected') must keep the affiliate's approved
// commissions OUT of payout batches; clearing it (received/validated/
// not_required — or no row at all) lets them back in automatically.

// setTaxDoc upserts a tax document status for an affiliate through the
// same shape the admin endpoint uses.
func setTaxDoc(t *testing.T, h *db.Handles, affID int64, docType, status string) {
	t.Helper()
	_, err := h.DB.Exec(`
		INSERT INTO tax_documents (affiliate_id, doc_type, status)
		VALUES (?, ?, ?)
		ON CONFLICT (affiliate_id, doc_type) DO UPDATE SET
			status = excluded.status`,
		affID, docType, status)
	if err != nil {
		t.Fatalf("set tax doc: %v", err)
	}
}

// TD1: blocking statuses are excluded from batch creation; non-blocking
// statuses and absent rows are included. The blocked affiliate's
// commissions stay 'approved' (nothing else mutates them).
func TestPayout_TaxDocGate_Batch(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()

	// Affiliate 1 blocked (pending W-9); affiliate 2 clean.
	matureOne(t, e, h, "td1-a", 1, 10000) // 1000 cents approved
	matureOne(t, e, h, "td1-b", 2, 20000) // 2000 cents approved
	setTaxDoc(t, h, 1, "w9", "pending")

	_, n, err := r.CreateBatch(ctx, 0, "paypal")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("payouts=%d want 1 (affiliate 1 blocked by pending tax doc)", n)
	}
	var blockedAff int64
	h.DB.QueryRow(`SELECT affiliate_id FROM payouts`).Scan(&blockedAff)
	if blockedAff != 2 {
		t.Fatalf("paid-out affiliate=%d want 2 (1 is tax-blocked)", blockedAff)
	}
	// Blocked affiliate's commissions are untouched: still approved.
	var still int64
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions
		WHERE affiliate_id=1 AND status='approved'`).Scan(&still)
	if still != 1 {
		t.Fatalf("blocked affiliate commissions=%d want 1 approved", still)
	}

	// Clearing the doc re-admits them on the next run.
	setTaxDoc(t, h, 1, "w9", "validated")
	_, n2, err := r.CreateBatch(ctx, 0, "paypal")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Fatalf("after validation payouts=%d want 1 (block must lift)", n2)
	}
	var aff int64
	h.DB.QueryRow(`SELECT affiliate_id FROM payouts WHERE affiliate_id=1`).Scan(&aff)
	if aff != 1 {
		t.Fatal("affiliate 1 not re-admitted after validation")
	}
}

// TD1b: 'rejected' blocks too; every non-blocking status admits.
func TestPayout_TaxDocGate_StatusMatrix(t *testing.T) {
	for _, tc := range []struct {
		status  string
		blocked bool
	}{
		{"pending", true},
		{"rejected", true},
		{"received", false},
		{"validated", false},
		{"not_required", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			h, r, e := newEnv(t)
			ctx := context.Background()
			matureOne(t, e, h, "td1b-"+tc.status, 1, 10000)
			setTaxDoc(t, h, 1, "w8ben", tc.status)
			_, n, err := r.CreateBatch(ctx, 0, "manual")
			if err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if tc.blocked {
				want = 0
			}
			if int64(n) != want {
				t.Fatalf("status %s: payouts=%d want %d", tc.status, n, want)
			}
		})
	}
}

// TD2: the Approve-time re-check. A doc recorded AFTER the batch was
// staged must block payment (payout stays 'requested'); once cleared,
// the same payout Approves and settles.
func TestPayout_TaxDocGate_ApproveRecheck(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "td2-a", 1, 10000)

	// Stage the batch while the affiliate is clean.
	_, n, err := r.CreateBatch(ctx, 0, "paypal")
	if err != nil || n != 1 {
		t.Fatalf("stage: n=%d err=%v", n, err)
	}
	var pid int64
	h.DB.QueryRow(`SELECT id FROM payouts`).Scan(&pid)

	// Operator records a pending doc AFTER staging.
	setTaxDoc(t, h, 1, "w9", "pending")
	if err := r.Approve(ctx, pid, nil); err == nil {
		t.Fatal("Approve paid a payout whose affiliate has a pending tax doc")
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id=?`, pid).Scan(&status)
	if status != "requested" {
		t.Fatalf("payout status=%s want requested (must survive the block)", status)
	}
	var commStatus string
	h.DB.QueryRow(`SELECT status FROM commissions`).Scan(&commStatus)
	if commStatus != "approved" {
		t.Fatalf("commission status=%s want approved (gate must not mutate money)", commStatus)
	}

	// Doc arrives: the SAME payout now approves and settles.
	setTaxDoc(t, h, 1, "w9", "received")
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatalf("Approve after doc received: %v", err)
	}
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id=?`, pid).Scan(&status)
	if status != "paid" {
		t.Fatalf("payout status=%s want paid", status)
	}
	var sum int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&sum)
	if sum != 0 {
		t.Fatalf("ledger sum=%d want 0", sum)
	}
}

// TD3 (NFR-9): every gate decision is auditable — status transitions on
// tax_documents land in the append-only audit_log on BOTH dialects.
func TestPayout_TaxDocGate_AuditTrail(t *testing.T) {
	h, _, _ := newEnv(t)
	setTaxDoc(t, h, 1, "w9", "pending")
	setTaxDoc(t, h, 1, "w9", "rejected")
	setTaxDoc(t, h, 1, "w9", "validated")

	var detail string
	err := h.DB.QueryRow(`SELECT detail FROM audit_log
		WHERE table_name='tax_documents' ORDER BY id DESC LIMIT 1`).Scan(&detail)
	if err != nil {
		t.Fatalf("no audit row for tax_documents transition: %v", err)
	}
	if detail != "rejected->validated" {
		t.Fatalf("latest audit detail=%q want rejected->validated", detail)
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE table_name='tax_documents'`).Scan(&n)
	if n != 2 { // pending->rejected, rejected->validated (INSERT is not a transition)
		t.Fatalf("audit rows=%d want 2", n)
	}
}

// TD4: gate survives DB-level enforcement — the CHECK constraints on
// tax_documents reject garbage statuses/types (both dialects).
func TestPayout_TaxDocGate_SchemaGuards(t *testing.T) {
	h, _, _ := newEnv(t)
	_, err := h.DB.Exec(`INSERT INTO tax_documents (affiliate_id, doc_type, status)
		VALUES (1, 'w9', 'garbage')`)
	if err == nil {
		t.Fatal("garbage status accepted by schema")
	}
	_, err = h.DB.Exec(`INSERT INTO tax_documents (affiliate_id, doc_type, status)
		VALUES (1, 'passport', 'pending')`)
	if err == nil {
		t.Fatal("garbage doc_type accepted by schema")
	}
}
