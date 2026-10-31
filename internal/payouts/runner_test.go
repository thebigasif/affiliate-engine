package payouts_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

func newEnv(t *testing.T) (*db.Handles, *payouts.Runner, *commissions.Engine) {
	t.Helper()
	ctx := context.Background()
	// Dual-engine: file-backed SQLite by default; PG via env (CI).
	h := dbtest.Open(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"AFFILIATE_PAYABLE:2:USD": "liability",
		"PAYOUT_CLEARING:USD":     "clearing",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1,'a@t.dev'), (2,'b@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value)
		VALUES (1, NULL, 'percent', 1000)`); err != nil {
		t.Fatal(err)
	}
	return h, payouts.NewRunner(h.DB), commissions.NewEngine(h.DB, p)
}

func mkEvent(id string, aff int64, amount int64) commissions.Event {
	return commissions.Event{
		Source: "stripe", ExternalID: id, EventType: "purchase",
		AmountMinor: amount, Currency: "USD",
		AffiliateID: aff, CampaignID: 1, OccurredAt: time.Now().UTC(),
	}
}

// matureOne credits + matures a single commission for an affiliate.
func matureOne(t *testing.T, e *commissions.Engine, h *db.Handles, id string, aff, amount int64) {
	t.Helper()
	ctx := context.Background()
	cid, err := e.ProcessPurchase(ctx, mkEvent(id, aff, amount))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE commissions SET matures_at=? WHERE id=?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), cid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Mature(ctx); err != nil {
		t.Fatal(err)
	}
}

// PY1: batch creation groups approved commissions per affiliate, threshold respected.
func TestPayout_BatchThreshold(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "py1-1", 1, 10000) // affiliate 1: 1000 comm
	matureOne(t, e, h, "py1-2", 2, 2000)  // affiliate 2: 200 comm (below threshold)

	runID, n, err := r.CreateBatch(ctx, 500, "paypal")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("payouts=%d want 1 (aff 2 below 500 threshold)", n)
	}
	var amount int64
	var status string
	if err := h.DB.QueryRow(`SELECT amount, status FROM payouts`).Scan(&amount, &status); err != nil {
		t.Fatal(err)
	}
	if amount != 1000 || status != "requested" {
		t.Fatalf("amount=%d status=%s", amount, status)
	}
	if err := r.CompleteRun(ctx, runID, "ok"); err != nil {
		t.Fatal(err)
	}
}

// PY2: whole-commission coverage — payout lines reference exact commission ids.
func TestPayout_WholeCommissions(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "py2-1", 1, 10000)
	matureOne(t, e, h, "py2-2", 1, 5000)
	_, n, err := r.CreateBatch(ctx, 0, "wise")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var lines int
	h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines`).Scan(&lines)
	if lines != 2 {
		t.Fatalf("lines=%d want 2 (whole commissions)", lines)
	}
	// Re-run: no new payouts (commissions already claimed).
	_, n2, err := r.CreateBatch(ctx, 0, "wise")
	if err != nil || n2 != 0 {
		t.Fatalf("re-run created %d new payouts (double-claim)", n2)
	}
}

// PY3: approve happy path marks payout + commissions paid.
func TestPayout_Approve(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "py3-1", 1, 10000)
	_, n, err := r.CreateBatch(ctx, 0, "paypal")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var pid int64
	h.DB.QueryRow(`SELECT id FROM payouts`).Scan(&pid)
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}
	var pStatus string
	var cPaid int
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id=?`, pid).Scan(&pStatus)
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status='paid'`).Scan(&cPaid)
	if pStatus != "paid" || cPaid != 1 {
		t.Fatalf("payout=%s commissionsPaid=%d", pStatus, cPaid)
	}
}

// PY4: executor failure reverts payout to requested (retryable), commissions stay approved.
func TestPayout_FailedExecution(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "py4-1", 1, 10000)
	_, _, err := r.CreateBatch(ctx, 0, "paypal")
	if err != nil {
		t.Fatal(err)
	}
	var pid int64
	h.DB.QueryRow(`SELECT id FROM payouts`).Scan(&pid)
	fail := func(int64, int64) error { return fmt.Errorf("transfer bounced") }
	if err := r.Approve(ctx, pid, fail); err == nil {
		t.Fatal("expected executor failure to surface")
	}
	var pStatus string
	var cApproved int
	h.DB.QueryRow(`SELECT status FROM payouts WHERE id=?`, pid).Scan(&pStatus)
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status='approved'`).Scan(&cApproved)
	if pStatus != "requested" {
		t.Fatalf("payout=%s want requested (retryable)", pStatus)
	}
	if cApproved != 1 {
		t.Fatalf("commissions must remain approved, got %d", cApproved)
	}
}

// PY5: ledger integration — approve posts payable->clearing and stays balanced.
func TestPayout_LedgerWired(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	matureOne(t, e, h, "py5-1", 1, 10000) // 1000 commission
	_, _, err := r.CreateBatch(ctx, 0, "paypal")
	if err != nil {
		t.Fatal(err)
	}
	// Wire the ledger poster.
	p := ledger.NewPoster(h.Ledger)
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := p.Post(ctx, fmt.Sprintf("payout:%d", payoutID), "payout", "", time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })

	var pid int64
	h.DB.QueryRow(`SELECT id FROM payouts`).Scan(&pid)
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}
	// Payable back to 0 (1000 credit matured in, 1000 paid out).
	bal, _ := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if bal != 0 {
		t.Fatalf("payable=%d want 0 after payout", bal)
	}
	if s, _ := p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}
