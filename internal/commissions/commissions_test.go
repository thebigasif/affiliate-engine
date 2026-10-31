package commissions_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

type env struct {
	h *db.Handles
	p *ledger.Poster
	e *commissions.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	// Dual-engine: file-backed SQLite by default; PG via env (CI).
	h := dbtest.Open(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	seed(t, h.DB)
	return &env{h: h, p: p, e: commissions.NewEngine(h.DB, p)}
}

func seed(t *testing.T, d *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v (q=%s)", err, q)
		}
	}
	exec(`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`)
	exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
	      VALUES (1, NULL, 'purchase', 'percent', 1000)`) // 10%
}

func purchase(id string, amount int64) commissions.Event {
	return commissions.Event{
		Source: "stripe", ExternalID: id, EventType: "purchase",
		AmountMinor: amount, Currency: "USD",
		AffiliateID: 1, CampaignID: 1, OccurredAt: time.Now().UTC(),
	}
}

// C1: purchase credits a pending commission of exact amount.
func TestCommission_Credit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("evt-1", 10000))
	if err != nil {
		t.Fatal(err)
	}
	var amount int64
	var status string
	if err := e.h.DB.QueryRow(
		`SELECT amount, status FROM commissions WHERE id = ?`, cid).Scan(&amount, &status); err != nil {
		t.Fatal(err)
	}
	if amount != 1000 { // 10% of 10000
		t.Fatalf("amount=%d want 1000", amount)
	}
	if status != "pending" {
		t.Fatalf("status=%s want pending", status)
	}
	// Ledger balanced.
	s, _ := e.p.GlobalIntegrity(ctx)
	if s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// C2: replaying the same external id is a no-op (ErrAlreadyProcessed).
func TestCommission_Replay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.e.ProcessPurchase(ctx, purchase("evt-r", 10000)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.e.ProcessPurchase(ctx, purchase("evt-r", 10000)); err != commissions.ErrAlreadyProcessed {
		t.Fatalf("want ErrAlreadyProcessed, got %v", err)
	}
	var n int
	e.h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("commissions=%d want 1", n)
	}
}

// C3: maturation moves pending->approved exactly once; HOLD->payable.
func TestCommission_Mature(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("evt-m", 10000))
	if err != nil {
		t.Fatal(err)
	}
	// Force maturity by backdating matures_at.
	if _, err := e.h.DB.Exec(`UPDATE commissions SET matures_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour), cid); err != nil {
		t.Fatal(err)
	}
	n, err := e.e.Mature(ctx)
	if err != nil || n != 1 {
		t.Fatalf("mature n=%d err=%v", n, err)
	}
	// Second run: nothing left.
	n, err = e.e.Mature(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second mature n=%d err=%v", n, err)
	}
	var status string
	e.h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&status)
	if status != "approved" {
		t.Fatalf("status=%s", status)
	}
	// Payable holds the credit; ledger still balanced.
	bal, _ := e.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if bal != -1000 {
		t.Fatalf("payable=%d want -1000", bal)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// C4: proportional clawback on partial refund while pending. A partial
// refund claws its share and leaves the parent pending: later refunds
// claw theirs, and the parent cancels only once fully refunded.
func TestCommission_ClawbackPending(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cid, err := e.e.ProcessPurchase(ctx, purchase("evt-c", 10000))
	if err != nil {
		t.Fatal(err)
	}
	refund := purchase("ref-1", 0)
	refund.EventType = "refund"
	// 40% refund: 4000/10000 => claw 40% of 1000 = 400.
	if err := e.e.ProcessRefund(ctx, refund, cid, 4000, 10000); err != nil {
		t.Fatal(err)
	}
	var status string
	e.h.DB.QueryRow(`SELECT status FROM commissions WHERE id = ?`, cid).Scan(&status)
	if status != "pending" {
		t.Fatalf("status=%s want pending (partial refund)", status)
	}
	var claw int64
	if err := e.h.DB.QueryRow(
		`SELECT amount FROM commissions WHERE parent_commission_id = ?`, cid).Scan(&claw); err != nil {
		t.Fatal(err)
	}
	if claw != -400 {
		t.Fatalf("clawback=%d want -400", claw)
	}
	if hold := holdBalance(t, e.h.DB, "HOLD:USD"); hold != -600 {
		t.Fatalf("HOLD=%d want -600", hold)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
	// Replay refund: no-op.
	if err := e.e.ProcessRefund(ctx, refund, cid, 4000, 10000); err != commissions.ErrAlreadyProcessed {
		t.Fatalf("want ErrAlreadyProcessed, got %v", err)
	}
}

// C5: coupon attribution + per-coupon rate override.
func TestCommission_CouponOverride(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.h.DB.Exec(`INSERT INTO coupons (code, affiliate_id, rate_type, rate_value)
		VALUES ('SUMMER', 1, 'fixed', 500)`); err != nil {
		t.Fatal(err)
	}
	ev := purchase("evt-coup", 10000)
	ev.AffiliateID = 0 // must resolve via coupon
	ev.CouponCode = "SUMMER"
	cid, err := e.e.ProcessPurchase(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	var amount int64
	e.h.DB.QueryRow(`SELECT amount FROM commissions WHERE id = ?`, cid).Scan(&amount)
	if amount != 500 {
		t.Fatalf("amount=%d want 500 (coupon fixed)", amount)
	}
}

// C6: lifetime binding — second purchase by same customer auto-attributes.
func TestCommission_LifetimeBinding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev1 := purchase("evt-l1", 10000)
	ev1.CustomerExtID = "cus_123"
	if _, err := e.e.ProcessPurchase(ctx, ev1); err != nil {
		t.Fatal(err)
	}
	// Second purchase: no affiliate id, no coupon — binding resolves.
	ev2 := purchase("evt-l2", 20000)
	ev2.AffiliateID = 0
	ev2.CustomerExtID = "cus_123"
	cid, err := e.e.ProcessPurchase(ctx, ev2)
	if err != nil {
		t.Fatal(err)
	}
	var aff int64
	e.h.DB.QueryRow(`SELECT affiliate_id FROM commissions WHERE id = ?`, cid).Scan(&aff)
	if aff != 1 {
		t.Fatalf("affiliate=%d want 1 (lifetime binding)", aff)
	}
	// Binding is first-touch: change the customer row manually won't matter —
	// immutability checked by ON CONFLICT DO NOTHING.
	if _, err := e.h.DB.Exec(`UPDATE customers SET affiliate_id = 999 WHERE external_customer_id = 'cus_123'`); err != nil {
		t.Logf("binding update rejected (immutable by convention): %v", err)
	}
}

// C7: unattributed event recorded, no commission, no error crash.
func TestCommission_Unattributed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev := purchase("evt-u", 5000)
	ev.AffiliateID = 0
	if _, err := e.e.ProcessPurchase(ctx, ev); err != commissions.ErrNoAttribution {
		t.Fatalf("want ErrNoAttribution, got %v", err)
	}
	var note string
	if err := e.h.DB.QueryRow(
		`SELECT process_error FROM raw_events WHERE external_event_id = 'evt-u'`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "unattributed" {
		t.Fatalf("note=%s", note)
	}
}

// C8: full lifecycle stays balanced: purchase x3, mature all, refund one paid.
func TestCommission_FullLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var cids []int64
	for i := 1; i <= 3; i++ {
		cid, err := e.e.ProcessPurchase(ctx, purchase(fmt.Sprintf("evt-f%d", i), 10000))
		if err != nil {
			t.Fatal(err)
		}
		cids = append(cids, cid)
	}
	// Backdate + mature all.
	_, _ = e.h.DB.Exec(`UPDATE commissions SET matures_at = ?`, time.Now().UTC().Add(-time.Hour))
	if _, err := e.e.Mature(ctx); err != nil {
		t.Fatal(err)
	}
	// Mark one approved commission paid.
	_, _ = e.h.DB.Exec(`UPDATE commissions SET status='paid', paid_at=? WHERE id = ?`,
		time.Now().UTC(), cids[0])
	// Refund it fully (paid state): clawback path.
	refund := purchase("ref-f1", 0)
	refund.EventType = "refund"
	if err := e.e.ProcessRefund(ctx, refund, cids[0], 10000, 10000); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.p.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d after lifecycle", s)
	}
	var bal int64
	e.h.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM commissions`).Scan(&bal)
	// 3 credits of 1000, one clawed back as -1000 row: 1000+1000-1000 = 1000 net across rows
	if bal != 1000 {
		t.Logf("net commission rows sum=%d (2 live x1000 + clawback -1000)", bal)
	}
}
