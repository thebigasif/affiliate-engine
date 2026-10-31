package httpapi

// INV-API-1 on the REAL money paths: the portal's ledger proof must hold at
// every step of a commission's life, not only on a hand-built fixture. The
// old claim (pending + approved == -payable) failed for every affiliate
// with a pending commission, because pending money sits in HOLD until it
// matures. These tests drive purchases, maturation, partial and full
// refunds, fraud upholds, payouts and post-payout refunds through the
// production code and check owed == -payable after each one.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
)

type proofEnv struct {
	t *testing.T
	h *db.Handles
	p *ledger.Poster
	e *commissions.Engine
	r *payouts.Runner
}

func newProofEnv(t *testing.T) *proofEnv {
	t.Helper()
	ctx := context.Background()
	h := dbtest.Open(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"PAYOUT_CLEARING:USD":     "clearing",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO affiliates (id, email) VALUES (1, 'a@t.dev')`,
		`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value)
		 VALUES (1, NULL, 'purchase', 'percent', 1000)`, // 10%
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := p.Post(ctx, payouts.SettleKey(payoutID), "payout", "", time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = p
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })
	return &proofEnv{t: t, h: h, p: p, e: commissions.NewEngine(h.DB, p), r: payouts.NewRunner(h.DB)}
}

func (pe *proofEnv) buy(id string, gross int64) int64 {
	pe.t.Helper()
	cid, err := pe.e.ProcessPurchase(context.Background(), commissions.Event{
		Source: "stripe", ExternalID: id, EventType: "purchase",
		AmountMinor: gross, Currency: "USD",
		AffiliateID: 1, CampaignID: 1, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		pe.t.Fatal(err)
	}
	return cid
}

func (pe *proofEnv) refund(id string, parent, refunded, original int64) {
	pe.t.Helper()
	if err := pe.e.ProcessRefund(context.Background(), commissions.Event{
		Source: "stripe", ExternalID: id, EventType: "refund", Currency: "USD",
		AffiliateID: 1, CampaignID: 1, OccurredAt: time.Now().UTC(),
	}, parent, refunded, original); err != nil {
		pe.t.Fatal(err)
	}
}

func (pe *proofEnv) mature(cid int64) {
	pe.t.Helper()
	if _, err := pe.h.DB.Exec(`UPDATE commissions SET matures_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), cid); err != nil {
		pe.t.Fatal(err)
	}
	if _, err := pe.e.Mature(context.Background()); err != nil {
		pe.t.Fatal(err)
	}
}

func (pe *proofEnv) payAll() {
	pe.t.Helper()
	ctx := context.Background()
	if _, _, err := pe.r.CreateBatch(ctx, 0, "manual"); err != nil {
		pe.t.Fatal(err)
	}
	rows, err := pe.h.DB.Query(`SELECT id FROM payouts WHERE status = 'requested'`)
	if err != nil {
		pe.t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			pe.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := pe.r.Approve(ctx, id, nil); err != nil {
			pe.t.Fatal(err)
		}
	}
}

// check asserts the proof and that it matches the expected owed amount.
func (pe *proofEnv) check(step string, wantOwed int64) {
	pe.t.Helper()
	ctx := context.Background()
	owedBy, err := owedFromCommissions(ctx, pe.h.DB, 1)
	if err != nil {
		pe.t.Fatal(err)
	}
	owed := owedBy["USD"]
	payable, err := pe.p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil {
		pe.t.Fatal(err)
	}
	if owed != -payable {
		pe.t.Fatalf("%s: proof broken: owed=%d, -payable=%d", step, owed, -payable)
	}
	if owed != wantOwed {
		pe.t.Fatalf("%s: owed=%d want %d", step, owed, wantOwed)
	}
	if s, _ := pe.p.GlobalIntegrity(ctx); s != 0 {
		pe.t.Fatalf("%s: ledger integrity %d", step, s)
	}
}

func (pe *proofEnv) holdBalance() int64 {
	pe.t.Helper()
	b, err := pe.p.AccountBalance(context.Background(), "HOLD:USD")
	if err != nil {
		pe.t.Fatal(err)
	}
	return b
}

// The reported bug: one pending commission made the portal say FAILED.
// Checked through the real earnings endpoint the portal calls.
func TestLedgerProof_PendingCommissionHolds(t *testing.T) {
	pe := newProofEnv(t)
	c := pe.buy("p1", 10000) // 1000 pending, in HOLD
	pe.check("pending", 0)

	key, err := apikeys.Store(context.Background(), pe.h.DB, "aff1", apikeys.AffiliateScope(1))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewAPIHandler(APIDeps{DB: pe.h.DB}))
	defer srv.Close()
	earnings := func() map[string]any {
		t.Helper()
		code, m := getJSON(t, srv.URL+"/api/v1/affiliate/earnings", key)
		if code != http.StatusOK {
			t.Fatalf("code=%d", code)
		}
		return m
	}
	m := earnings()
	proof := m["ledger_proof"].(map[string]any)
	if proof["holds"] != true {
		t.Fatalf("portal shows FAILED with only a pending commission: %v", m)
	}
	if m["unpaid_cents"].(float64) != 1000 {
		t.Fatalf("unpaid=%v want 1000", m["unpaid_cents"])
	}

	pe.mature(c)
	proof = earnings()["ledger_proof"].(map[string]any)
	if proof["holds"] != true || proof["owed_cents"].(float64) != 1000 {
		t.Fatalf("after maturation: %v", proof)
	}
}

func TestLedgerProof_FullLifecycle(t *testing.T) {
	pe := newProofEnv(t)

	c1 := pe.buy("c1", 10000) // 1000
	c2 := pe.buy("c2", 20000) // 2000
	c3 := pe.buy("c3", 30000) // 3000
	pe.check("three pending", 0)

	// Partial refund while pending: claw comes out of HOLD.
	pe.refund("r1", c1, 5000, 10000) // claws 500 of c1
	pe.check("partial refund while pending", 0)

	pe.mature(c1) // moves the 500 remainder
	pe.check("c1 matured after partial refund", 500)

	pe.mature(c2)
	pe.check("c2 matured", 2500)

	// Partial refund on an approved commission: claw out of the payable.
	pe.refund("r2", c2, 5000, 20000) // claws 500 of c2
	pe.check("partial refund while approved", 2000)

	// Payout nets the clawbacks: pays 500 + 1500 = 2000.
	pe.payAll()
	pe.check("paid out", 0)
	var paid int64
	if err := pe.h.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM payouts WHERE status='paid'`).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 2000 {
		t.Fatalf("paid=%d want 2000", paid)
	}

	// Refund after payout: the affiliate now owes us (payable positive).
	pe.refund("r3", c2, 15000, 20000) // rest of c2: claws 1500
	pe.check("refund after payout", -1500)

	// New earnings net the receivable at the next payout.
	pe.mature(c3)
	pe.check("c3 matured", 1500)
	pe.payAll()
	pe.check("second payout nets the receivable", 0)
}

// Fraud uphold after a partial refund must claw only what is left. It
// used to claw the full amount again (the refunded part twice).
func TestLedgerProof_UpholdAfterPartialRefund(t *testing.T) {
	ctx := context.Background()

	t.Run("approved", func(t *testing.T) {
		pe := newProofEnv(t)
		keep := pe.buy("keep", 50000) // 5000, keeps the payable funded
		pe.mature(keep)
		c := pe.buy("u1", 10000) // 1000
		pe.mature(c)
		pe.refund("ur1", c, 4000, 10000) // claws 400 from payable
		pe.check("before uphold", 5600)
		if _, err := pe.h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'test')`, c); err != nil {
			t.Fatal(err)
		}
		if err := fraud.Uphold(ctx, pe.h.DB, pe.p, c, "fraud"); err != nil {
			t.Fatal(err)
		}
		pe.check("after uphold", 5000) // only the 600 remainder clawed
	})

	t.Run("pending", func(t *testing.T) {
		pe := newProofEnv(t)
		c := pe.buy("u2", 10000) // 1000 in HOLD
		pe.refund("ur2", c, 4000, 10000)
		if got := pe.holdBalance(); got != -600 {
			t.Fatalf("HOLD=%d want -600", got)
		}
		if _, err := pe.h.DB.Exec(`INSERT INTO commission_holds (commission_id, reason) VALUES (?, 'test')`, c); err != nil {
			t.Fatal(err)
		}
		if err := fraud.Uphold(ctx, pe.h.DB, pe.p, c, "fraud"); err != nil {
			t.Fatal(err)
		}
		if got := pe.holdBalance(); got != 0 {
			t.Fatalf("HOLD=%d want 0 (uphold over-drained the hold)", got)
		}
		pe.check("after pending uphold", 0)
	})

	t.Run("fully refunded then upheld", func(t *testing.T) {
		pe := newProofEnv(t)
		keep := pe.buy("keep2", 50000)
		pe.mature(keep)
		c := pe.buy("u3", 10000)
		pe.mature(c)
		pe.refund("ur3", c, 10000, 10000) // full refund: parent cancelled
		pe.check("fully refunded", 5000)
		// Already terminal: uphold rejects, nothing moves.
		if err := fraud.Uphold(ctx, pe.h.DB, pe.p, c, "fraud"); err == nil {
			t.Fatal("uphold on a cancelled commission should be rejected")
		}
		pe.check("after rejected uphold", 5000)
	})
}

// The payout preview must be exactly what the real run then creates,
// including clawback netting and one payout per currency.
func TestPayoutPreviewMatchesRun(t *testing.T) {
	ctx := context.Background()
	pe := newProofEnv(t)
	c1 := pe.buy("pv1", 10000)
	c2 := pe.buy("pv2", 20000)
	pe.mature(c1)
	pe.mature(c2)
	pe.refund("pvr", c2, 10000, 20000) // claws 1000 of c2 (approved)
	// A EUR commission for the same affiliate: its own payout.
	if _, err := pe.h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, event_type, rate_type, rate_value, priority)
		VALUES (2, NULL, 'purchase', 'percent', 1000, 0)`); err != nil {
		t.Fatal(err)
	}
	ceur, err := pe.e.ProcessPurchase(ctx, commissions.Event{
		Source: "stripe", ExternalID: "pveur", EventType: "purchase",
		AmountMinor: 40000, Currency: "EUR", AffiliateID: 1, CampaignID: 2, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pe.mature(ceur)

	preview, err := pe.r.Preview(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, p := range preview {
		got[p.Currency] = p.Cents
	}
	if got["USD"] != 2000 || got["EUR"] != 4000 || len(preview) != 2 {
		t.Fatalf("preview=%+v want USD 2000 (3000 - 1000 clawback) and EUR 4000", preview)
	}
	if _, _, err := pe.r.CreateBatch(ctx, 0, "manual"); err != nil {
		t.Fatal(err)
	}
	rows, err := pe.h.DB.Query(`SELECT currency, amount FROM payouts`)
	if err != nil {
		t.Fatal(err)
	}
	real := map[string]int64{}
	for rows.Next() {
		var cur string
		var amt int64
		if err := rows.Scan(&cur, &amt); err != nil {
			t.Fatal(err)
		}
		real[cur] = amt
	}
	_ = rows.Close()
	if len(real) != len(got) || real["USD"] != got["USD"] || real["EUR"] != got["EUR"] {
		t.Fatalf("run %v != preview %v", real, got)
	}
}
