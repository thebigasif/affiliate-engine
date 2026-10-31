package screening_test

// End-to-end against the real commissions + fraud engines: the commission
// that TRIGGERS a verdict is the one held (self-referral used to credit
// untouched because screening ran before the insert), click_velocity
// fires from the originating click's ip_hash, and dry-run holds nothing.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

func realEnv(t *testing.T, dryRun bool) (*db.Handles, *screening.Engine) {
	t.Helper()
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
	return h, screening.New(inner, fraud.NewEngine(h.DB), dryRun)
}

func purchase(id, email, click string) commissions.Event {
	return commissions.Event{
		Source: "test", ExternalID: id, EventType: "purchase",
		AmountMinor: 10000, Currency: "USD", AffiliateID: 1, CampaignID: 1,
		CustomerEmail: email, ClickID: click, OccurredAt: time.Now().UTC(),
	}
}

func holdReason(t *testing.T, h *db.Handles, cid int64) string {
	t.Helper()
	var reason string
	if err := h.DB.QueryRow(`SELECT reason FROM commission_holds
		WHERE commission_id = ? AND resolved_at IS NULL`, cid).Scan(&reason); err != nil {
		return ""
	}
	return reason
}

func TestScreening_SelfReferralHoldsTriggeringCommission(t *testing.T) {
	h, e := realEnv(t, false)
	cid, err := e.ProcessPurchase(context.Background(), purchase("sr-1", "o.w.n.e.r+x@gmail.com", ""))
	if err != nil || cid <= 0 {
		t.Fatalf("cid=%d err=%v", cid, err)
	}
	if r := holdReason(t, h, cid); r == "" {
		t.Fatal("self-referral did not hold the commission it triggered")
	}
	// A clean purchase for the same affiliate is not held.
	clean, err := e.ProcessPurchase(context.Background(), purchase("sr-2", "buyer@else.dev", ""))
	if err != nil {
		t.Fatal(err)
	}
	if r := holdReason(t, h, clean); r != "" {
		t.Fatalf("clean commission held: %s", r)
	}
}

func TestScreening_ClickVelocityFromClickIPHash(t *testing.T) {
	h, e := realEnv(t, false)
	ip := []byte{4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4}
	now := time.Now().UTC()
	for i := 0; i < 55; i++ {
		if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
			VALUES (?, ?, ?, ?, 1, 1, ?)`,
			fmt.Sprintf("vc%03d", i), now.Add(-5*time.Minute).Format(time.RFC3339Nano),
			now.Format("20060102"), now.Format("200601"), ip); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := e.ProcessPurchase(context.Background(), purchase("cv-1", "buyer@else.dev", "vc010"))
	if err != nil || cid <= 0 {
		t.Fatalf("cid=%d err=%v", cid, err)
	}
	r := holdReason(t, h, cid)
	if r == "" {
		t.Fatal("click_velocity did not fire from the click's ip_hash")
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM fraud_verdicts WHERE rule = 'click_velocity'`).Scan(&n)
	if n == 0 {
		t.Fatalf("no click_velocity verdict recorded (hold reason %q)", r)
	}
}

func TestScreening_DryRunIntegrationNeverHolds(t *testing.T) {
	h, e := realEnv(t, true)
	if _, err := e.ProcessPurchase(context.Background(), purchase("dr-1", "owner@gmail.com", "")); err != nil {
		t.Fatal(err)
	}
	var holds, verdicts int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds`).Scan(&holds)
	h.DB.QueryRow(`SELECT COUNT(*) FROM fraud_verdicts WHERE dry_run = TRUE`).Scan(&verdicts)
	if holds != 0 {
		t.Fatalf("dry-run held %d commissions", holds)
	}
	if verdicts == 0 {
		t.Fatal("dry-run recorded no verdicts")
	}
}
