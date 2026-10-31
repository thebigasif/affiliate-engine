package commissions_test

import (
	"context"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/mlm"
)

// PRD gap #4 pin: MLMBV hook — a credited commission automatically records
// personal BV for the affiliate and group BV up the sponsor tree. The tree
// previously existed but dollars never reached it without manual endpoint
// choreography.
func TestCommission_RecordsMLMBV(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Sponsor tree: affiliate 2 under 1 (so affiliate 1 has downstream).
	mlmEng := mlm.New(e.h.DB)
	// seed() inserts affiliate 1 only; affiliate 2 must exist first.
	if _, err := e.h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (2, 'b2@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if err := mlmEng.PlaceInSponsorTree(ctx, 2, 1); err != nil {
		t.Fatalf("place 2 under 1: %v", err)
	}
	e.e.MLMBV = mlmEng.RecordBV

	// Affiliate 1's own sale of 10000 -> commission 1000 = BV.
	if _, err := e.e.ProcessPurchase(ctx, purchase("bv-evt", 10000)); err != nil {
		t.Fatal(err)
	}
	var personal, group int64
	if err := e.h.DB.QueryRow(`
		SELECT bv_personal, bv_group FROM mlm_volume_daily
		WHERE affiliate_id = 1 ORDER BY day DESC LIMIT 1`).Scan(&personal, &group); err != nil {
		t.Fatalf("no BV row for affiliate 1: %v (hook did not fire)", err)
	}
	if personal != 1000 {
		t.Fatalf("personal BV=%d want 1000", personal)
	}
	if group != 0 {
		t.Fatalf("group BV=%d want 0 (affiliate 1 has no ancestors)", group)
	}

	// A yen sale is not program-currency volume: BV is unchanged.
	if _, err := e.e.ProcessPurchase(ctx, jpyPurchase("bv-evt-jpy", 100000)); err != nil {
		t.Fatal(err)
	}
	if err := e.h.DB.QueryRow(`SELECT COALESCE(SUM(bv_personal), 0) FROM mlm_volume_daily
		WHERE affiliate_id = 1`).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	if personal != 1000 {
		t.Fatalf("personal BV after a yen sale=%d want 1000", personal)
	}
}
