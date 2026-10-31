package commissions_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// Per-currency money: a sale only counts toward, and only uses, rules and
// tiers in its own currency. The program currency (USD here) is what a
// rule or tier without an explicit currency is denominated in.

func jpyPurchase(id string, amount int64) commissions.Event {
	ev := purchase(id, amount)
	ev.Currency = "JPY"
	return ev
}

// A USD $200.00 ladder is not crossed by ¥20,000 of yen sales: yen volume
// neither counts toward the USD ladder nor unlocks its rate (no FX).
func TestPerCurrency_TierIgnoresOtherCurrencyVolume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedTier(t, e, "", 20000, 2500) // >= $200.00 referred -> 25%

	if _, err := e.e.ProcessPurchase(ctx, jpyPurchase("jt-1", 20000)); err != nil {
		t.Fatal(err)
	}
	// USD volume is still 0: the next USD sale earns the base 10%.
	cid, err := e.e.ProcessPurchase(ctx, tierPurchase("jt-usd", "", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("USD sale after yen volume: amount=%d want 1000 (base 10%%; yen must not cross a USD tier)", got)
	}
	// A yen sale never uses the USD ladder, however much yen came before.
	cid2, err := e.e.ProcessPurchase(ctx, jpyPurchase("jt-2", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid2); got != 1000 {
		t.Fatalf("yen sale: amount=%d want 1000 (base 10%%, not the USD tier)", got)
	}
}

// A fixed 500 (= $5.00) rule must not pay ¥500 on a yen sale: a fixed
// rule in another currency is skipped; with no other rule the sale earns
// nothing and is recorded as a currency mismatch.
func TestPerCurrency_FixedRuleOtherCurrencySkipped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`UPDATE commission_rules SET rate_type = 'fixed', rate_value = 500`); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, jpyPurchase("jf-1", 100000))
	if err != nil {
		t.Fatal(err)
	}
	if cid != 0 {
		t.Fatalf("fixed USD rule paid %d on a JPY sale", commissionAmount(t, e, cid))
	}
	var note string
	if err := e.h.DB.QueryRow(`SELECT COALESCE(process_error, '') FROM raw_events WHERE external_event_id = 'jf-1'`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "currency_mismatch" {
		t.Fatalf("raw event note=%q want currency_mismatch", note)
	}
	// The USD sale still earns the fixed $5.00.
	cid, err = e.e.ProcessPurchase(ctx, purchase("jf-usd", 100000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 500 {
		t.Fatalf("USD fixed: amount=%d want 500", got)
	}
}

// A fixed rule in the sale's currency applies: an affiliate override
// "fixed ¥800" pays the yen sale, and a USD sale falls through it to the
// USD campaign default.
func TestPerCurrency_FixedRuleInSaleCurrencyApplies(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value, currency)
		VALUES (1, 1, 'purchase', 'fixed', 800, 'JPY')`); err != nil {
		t.Fatal(err)
	}
	cid, err := e.e.ProcessPurchase(ctx, jpyPurchase("jy-1", 100000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 800 {
		t.Fatalf("JPY fixed: amount=%d want 800", got)
	}
	cid, err = e.e.ProcessPurchase(ctx, purchase("jy-usd", 10000))
	if err != nil {
		t.Fatal(err)
	}
	if got := commissionAmount(t, e, cid); got != 1000 {
		t.Fatalf("USD sale past a JPY fixed override: amount=%d want 1000 (10%% default)", got)
	}
}
