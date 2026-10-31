package screening_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

// brokenRule fails with a real statement error inside the transaction
// (on Postgres that aborts the transaction unless it is contained).
type brokenRule struct{ db fraud.Querier }

func (brokenRule) Name() string { return "broken" }

func (r brokenRule) Evaluate(ctx context.Context, q fraud.Query) ([]fraud.Verdict, error) {
	var n int
	err := fraud.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT COUNT(*) FROM no_such_table`).Scan(&n)
	return nil, err
}

// A rule that errors still fails closed, in the same transaction: the
// commission credits held "screening_error", and the rules that worked
// keep their verdicts (on Postgres the failed statement is undone alone).
func TestScreening_RuleErrorHoldsInTransaction(t *testing.T) {
	h := dbtest.Open(t)
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'owner@gmail.com')`,
		`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value) VALUES (1, NULL, 'percent', 1000)`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	inner := commissions.NewEngine(h.DB, ledger.NewPoster(h.Ledger))
	// The built-in rule reads through the transaction (fraud.Conn).
	fe := fraud.NewEngineWithRules(h.DB, brokenRule{h.DB}, fraud.SelfReferral{})
	e := screening.New(inner, fe, false)

	clean, err := e.ProcessPurchase(context.Background(), purchase("re-1", "buyer@else.dev", ""))
	if err != nil || clean <= 0 {
		t.Fatalf("cid=%d err=%v", clean, err)
	}
	if r := holdReason(t, h, clean); r != screening.ScreeningErrorReason {
		t.Fatalf("hold reason %q, want %q", r, screening.ScreeningErrorReason)
	}

	flagged, err := e.ProcessPurchase(context.Background(), purchase("re-2", "owner@gmail.com", ""))
	if err != nil || flagged <= 0 {
		t.Fatalf("cid=%d err=%v", flagged, err)
	}
	if r := holdReason(t, h, flagged); r != "fraud review: self_referral" {
		t.Fatalf("hold reason %q, want the working rule's", r)
	}
	if n := count(t, h, `SELECT COUNT(*) FROM fraud_verdicts WHERE rule = 'self_referral'`); n != 1 {
		t.Fatalf("self_referral verdicts = %d, want 1", n)
	}
}
