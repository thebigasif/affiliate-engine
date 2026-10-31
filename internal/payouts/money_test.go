package payouts_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

// creditMature credits + matures one event and returns the commission id.
func creditMature(t *testing.T, e *commissions.Engine, h *db.Handles, ev commissions.Event) int64 {
	t.Helper()
	ctx := context.Background()
	cid, err := e.ProcessPurchase(ctx, ev)
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
	return cid
}

// wireLedger mirrors cmd/affiliate-engine/main.go payout wiring.
func wireLedger(t *testing.T, h *db.Handles) *ledger.Poster {
	t.Helper()
	p := ledger.NewPoster(h.Ledger)
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := p.Post(ctx, fmt.Sprintf("payout:%d", payoutID), "payout", "", time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })
	return p
}

func refundOf(id string) commissions.Event {
	ev := mkEvent(id, 1, 0)
	ev.EventType = "refund"
	return ev
}

func onlyPayout(t *testing.T, h *db.Handles, status string) (id, amount int64) {
	t.Helper()
	if err := h.DB.QueryRow(`SELECT id, amount FROM payouts WHERE status = ?`, status).Scan(&id, &amount); err != nil {
		t.Fatalf("payout in %s: %v", status, err)
	}
	return id, amount
}

// Fix 5: payouts group by (affiliate, currency) and carry the real
// currency — USD and EUR are never summed into one 'USD' payout.
func TestPayout_GroupsByCurrency(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	creditMature(t, e, h, mkEvent("cur-usd", 1, 10000)) // 1000 USD
	eur := mkEvent("cur-eur", 1, 20000)                 // 2000 EUR
	eur.Currency = "EUR"
	creditMature(t, e, h, eur)

	_, n, err := r.CreateBatch(ctx, 0, "manual")
	if err != nil || n != 2 {
		t.Fatalf("payouts=%d err=%v want 2 (one per currency)", n, err)
	}
	rows, err := h.DB.Query(`SELECT currency, amount FROM payouts ORDER BY currency`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var c string
		var a int64
		if err := rows.Scan(&c, &a); err != nil {
			t.Fatal(err)
		}
		got[c] = a
	}
	if got["USD"] != 1000 || got["EUR"] != 2000 || len(got) != 2 {
		t.Fatalf("payouts=%v want USD:1000 EUR:2000", got)
	}
}

// Fix 2: a refund after payout leaves a receivable that the next batch
// nets against new earnings; the books close at zero.
func TestPayout_ClawbackAfterPayoutNetted(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	p := wireLedger(t, h)

	c1 := creditMature(t, e, h, mkEvent("cn-1", 1, 10000)) // 1000
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("batch 1: n=%d err=%v", n, err)
	}
	pid, _ := onlyPayout(t, h, "requested")
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}

	// Full refund after payout: must claw (payable goes +1000).
	if err := e.ProcessRefund(ctx, refundOf("cn-ref"), c1, 10000, 10000); err != nil {
		t.Fatalf("post-payout refund: %v", err)
	}
	if bal, _ := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD"); bal != 1000 {
		t.Fatalf("payable=%d want +1000 (receivable)", bal)
	}
	// Nothing else owed: no payout.
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 0 {
		t.Fatalf("receivable-only batch: n=%d err=%v want 0", n, err)
	}

	// New earnings of 2000: the batch pays 2000 - 1000 = 1000.
	creditMature(t, e, h, mkEvent("cn-2", 1, 20000))
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("netting batch: n=%d err=%v", n, err)
	}
	pid2, amount := onlyPayout(t, h, "requested")
	if amount != 1000 {
		t.Fatalf("netted payout=%d want 1000", amount)
	}
	if err := r.Approve(ctx, pid2, nil); err != nil {
		t.Fatalf("approve netted payout: %v", err)
	}
	if bal, _ := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD"); bal != 0 {
		t.Fatalf("payable=%d want 0", bal)
	}
	if s, _ := p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
	if _, n, _ := r.CreateBatch(ctx, 0, "manual"); n != 0 {
		t.Fatalf("re-batch staged %d payouts (clawback netted twice?)", n)
	}
}

// Fix 2/4: a partial refund landing after staging makes Approve release
// the payout (never pay more than owed); the restaged batch pays the net.
func TestPayout_RefundAfterStagingRestages(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	p := wireLedger(t, h)

	c1 := creditMature(t, e, h, mkEvent("rs-1", 1, 10000)) // 1000
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	pid, _ := onlyPayout(t, h, "requested")

	if err := e.ProcessRefund(ctx, refundOf("rs-ref"), c1, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	err := r.Approve(ctx, pid, nil)
	if !errors.Is(err, payouts.ErrRestaged) {
		t.Fatalf("approve after refund: %v (want ErrRestaged)", err)
	}
	var lines int
	h.DB.QueryRow(`SELECT COUNT(*) FROM payout_lines WHERE payout_id = ?`, pid).Scan(&lines)
	if lines != 0 {
		t.Fatalf("restaged payout kept %d lines", lines)
	}

	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("restage batch: n=%d err=%v", n, err)
	}
	pid2, amount := onlyPayout(t, h, "requested")
	if amount != 600 {
		t.Fatalf("restaged payout=%d want 600 (1000 - 400 clawed)", amount)
	}
	if err := r.Approve(ctx, pid2, nil); err != nil {
		t.Fatal(err)
	}
	if bal, _ := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD"); bal != 0 {
		t.Fatalf("payable=%d want 0", bal)
	}
	if s, _ := p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}
