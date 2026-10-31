package payouts_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// A settle whose commission write fails used to have already posted the
// payable -> clearing journal and flipped the payout to 'paid': the books
// said paid while the member commission stayed 'approved' (and eligible
// for the next batch). Journal, payout row and commission rows now commit
// together, so the failure moves nothing and the retry pays once.
func TestAtomic_SettleCommissionFailureMovesNothing(t *testing.T) {
	h, r, e := newEnv(t)
	ctx := context.Background()
	p := wireLedger(t, h)
	cid := creditMature(t, e, h, mkEvent("atom-pay", 1, 10000))
	if _, n, err := r.CreateBatch(ctx, 0, "manual"); err != nil || n != 1 {
		t.Fatalf("batch n=%d err=%v", n, err)
	}
	pid, amount := onlyPayout(t, h, "requested")
	payBefore, err := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil {
		t.Fatal(err)
	}

	name := "inject_paid"
	if dbtest.IsPostgres() {
		if _, err := h.DB.Exec(`CREATE OR REPLACE FUNCTION ae_inject() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON commissions
			FOR EACH ROW WHEN (NEW.status = 'paid') EXECUTE FUNCTION ae_inject()`); err != nil {
			t.Fatal(err)
		}
	} else if _, err := h.DB.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON commissions
		WHEN NEW.status = 'paid' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}

	if err := r.Approve(ctx, pid, nil); err == nil {
		t.Fatal("injected commission-write failure not reported")
	}
	var status string
	if err := h.DB.QueryRow(`SELECT status FROM payouts WHERE id = ?`, pid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "paid" {
		t.Errorf("payout marked paid while its commission write failed")
	}
	var settled int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE idem_key = ?`,
		fmt.Sprintf("payout:%d", pid)).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != 0 {
		t.Errorf("settle journal posted for a payout whose commission write failed")
	}
	if bal, _ := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD"); bal != payBefore {
		t.Errorf("payable %d -> %d on a failed settle", payBefore, bal)
	}

	if dbtest.IsPostgres() {
		_, err = h.DB.Exec(`DROP TRIGGER ` + name + ` ON commissions`)
	} else {
		_, err = h.DB.Exec(`DROP TRIGGER ` + name)
	}
	if err != nil {
		t.Fatal(err)
	}
	if status != "requested" {
		t.Fatalf("payout status=%q after a failed manual settle, want requested (retryable)", status)
	}
	if err := r.Approve(ctx, pid, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var cStatus string
	if err := h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&cStatus); err != nil {
		t.Fatal(err)
	}
	if cStatus != "paid" {
		t.Fatalf("commission status=%s after retry, want paid", cStatus)
	}
	if bal, _ := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD"); bal != payBefore+amount {
		t.Fatalf("payable=%d after retry, want %d (paid once)", bal, payBefore+amount)
	}
}
