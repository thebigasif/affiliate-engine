package commissions_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
)

// Two sales for one partner on one day both count in daily_agg (the
// Overview chart), on every engine: the second takes the UPDATE branch.
func TestDailyAgg_TwoSalesSameDay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, id := range []string{"evt-d1", "evt-d2"} {
		if _, err := e.e.ProcessPurchase(ctx, purchase(id, 10000)); err != nil {
			t.Fatal(err)
		}
	}
	var conv, cents int64
	if err := e.h.DB.QueryRow(`SELECT COALESCE(SUM(conversions),0), COALESCE(SUM(commission_cents),0) FROM daily_agg`).Scan(&conv, &cents); err != nil {
		t.Fatal(err)
	}
	if conv != 2 || cents != 2000 {
		t.Fatalf("daily_agg conversions=%d commission_cents=%d, want 2 and 2000", conv, cents)
	}
}

// A dollar sale and two yen sales on one day for one partner are one
// daily_agg row per currency: the aggregate never adds yen to cents.
func TestDailyAgg_PerCurrency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, ev := range []commissions.Event{
		purchase("evt-pc-usd", 10000), jpyPurchase("evt-pc-jpy", 20000), jpyPurchase("evt-pc-jpy2", 10000),
	} {
		if _, err := e.e.ProcessPurchase(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := e.h.DB.Query(`SELECT currency, conversions, commission_cents FROM daily_agg
		WHERE conversions <> 0 OR commission_cents <> 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]int64{}
	for rows.Next() {
		var c string
		var n, v int64
		if err := rows.Scan(&c, &n, &v); err != nil {
			t.Fatal(err)
		}
		got[c] = [2]int64{n, v}
	}
	if len(got) != 2 || got["USD"] != [2]int64{1, 1000} || got["JPY"] != [2]int64{2, 3000} {
		t.Fatalf("daily_agg money rows = %v, want USD {1 1000} and JPY {2 3000}", got)
	}
}
