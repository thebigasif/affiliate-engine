package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// An orphan settle journal — posted by a release that settled outside one
// transaction, whose domain writes then failed — followed by more usage
// before the retry: the retry must finish the posted journal, not post the
// full delta again under a new key and leave the first journal in HOLD
// with no commission.
func TestUsage_RetryAfterMoreUsageNoOrphanJournal(t *testing.T) {
	h, ing, set, poster := newEnv(t)
	ctx := context.Background()
	const day = int64(20260915)
	if err := ing.Ingest(ctx, ev("r1", 100000)); err != nil { // basis 3000 at 300 bp
		t.Fatal(err)
	}
	if _, err := poster.Post(ctx, "usage:20260915:cus_ai:tokens:from:0", "usage_settlement", "cus_ai",
		time.Unix(0, 0).UTC(), []ledger.Line{
			{Account: "EXPENSE:COMMISSION:USD", Amount: 3000},
			{Account: "HOLD:USD", Amount: -3000},
		}); err != nil {
		t.Fatal(err)
	}

	if err := ing.Ingest(ctx, ev("r2", 50000)); err != nil { // basis now 4500
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := set.SettleDay(ctx, day, 300); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}

	var total, settled int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM commissions`).Scan(&total)
	h.DB.QueryRow(`SELECT settled_cents FROM usage_daily WHERE customer_ext_id = 'cus_ai'`).Scan(&settled)
	expense, err := poster.AccountBalance(ctx, "EXPENSE:COMMISSION:USD")
	if err != nil {
		t.Fatal(err)
	}
	if total != 4500 || settled != 4500 {
		t.Fatalf("commissions=%d settled=%d, want 4500 each", total, settled)
	}
	if expense != total {
		t.Fatalf("ledger expensed %d but commissions total %d: a journal sits in HOLD with no commission", expense, total)
	}
}

// A settle whose commission row fails used to have already posted its
// journal on the ledger connection: HOLD carried the liability with no
// commission and settled_cents un-advanced. The journals, rows and
// settled markers now commit together.
func TestAtomic_UsageSettleRowFailureMovesNothing(t *testing.T) {
	h, ing, set, poster := newEnv(t)
	ctx := context.Background()
	const day = int64(20260915)
	if err := ing.Ingest(ctx, ev("a1", 100000)); err != nil { // basis 3000 at 300 bp
		t.Fatal(err)
	}
	name := "inject_usage"
	if dbtest.IsPostgres() {
		if _, err := h.DB.Exec(`CREATE OR REPLACE FUNCTION ae_inject() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT ON commissions
			FOR EACH ROW EXECUTE FUNCTION ae_inject()`); err != nil {
			t.Fatal(err)
		}
	} else if _, err := h.DB.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT ON commissions
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := set.SettleDay(ctx, day, 300); err == nil {
		t.Fatal("injected commission-row failure not reported")
	}
	var journals int
	h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE source = 'usage_settlement'`).Scan(&journals)
	if journals != 0 {
		t.Errorf("%d usage journal(s) posted by a settle whose commission rows never landed", journals)
	}
	if hold, _ := poster.AccountBalance(ctx, "HOLD:USD"); hold != 0 {
		t.Errorf("HOLD=%d after a failed settle, want 0", hold)
	}

	var err error
	if dbtest.IsPostgres() {
		_, err = h.DB.Exec(`DROP TRIGGER ` + name + ` ON commissions`)
	} else {
		_, err = h.DB.Exec(`DROP TRIGGER ` + name)
	}
	if err != nil {
		t.Fatal(err)
	}
	if n, err := set.SettleDay(ctx, day, 300); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if hold, _ := poster.AccountBalance(ctx, "HOLD:USD"); hold != -3000 {
		t.Fatalf("HOLD=%d after retry, want -3000", hold)
	}
}
