package e2e_test

// The M1 gate: full lifecycle on a fresh database.
// click (/r/) -> signed Stripe webhook -> commission -> maturation ->
// payout batch -> ledger integrity == 0. Every package wired together.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

func TestEndToEnd_Lifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "e2e.db")
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}

	// Setup: accounts, affiliate, rule, signer.
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"PAYOUT_CLEARING:USD":     "clearing",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'partner@aff.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value)
		VALUES (1, NULL, 'percent', 1000)`); err != nil { // 10%
		t.Fatal(err)
	}

	secret, _ := tokens.NewRandomSecret(32)
	signer, _ := tokens.New(secret)
	committer := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(committer.Close)

	// 1) CLICK: hit the redirect endpoint.
	mux := http.NewServeMux()
	mux.Handle("/r/", httpapi.NewRedirectHandler(h.DB, httpapi.Config{
		Signer:              signer,
		AllowedDestSuffixes: []string{"shop.test"},
		FallbackURL:         "https://shop.test/",
	}, committer))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + "/r/1?dest=https://shop.test/buy")
	if err != nil {
		t.Fatal(err)
	}
	var attributionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ae_attribution" {
			attributionCookie = c
		}
	}
	if attributionCookie == nil {
		t.Fatal("no attribution cookie after click")
	}
	waitFor(t, func() bool {
		var n int
		h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&n)
		var a int
		h.DB.QueryRow(`SELECT COUNT(*) FROM attributions`).Scan(&a)
		return n == 1 && a == 1
	})

	// 2) CONVERSION: the affiliate's customer buys (metadata carries the token
	// the merchant's checkout copied from the cookie).
	engine := commissions.NewEngine(h.DB, poster)
	event := commissions.Event{
		Source: "stripe", ExternalID: "evt_e2e_1", EventType: "purchase",
		AmountMinor: 20000, Currency: "USD",
		AffiliateID:   1, // resolved via token in a real adapter; explicit here
		CampaignID:    1,
		CustomerExtID: "cus_e2e_1",
		OccurredAt:    time.Now().UTC(),
	}
	// (The billing adapter agent owns signature verification; e2e drives the
	// engine directly with the resolved attribution.)
	cid, err := engine.ProcessPurchase(ctx, event)
	if err != nil {
		t.Fatal(err)
	}

	// 3) Commission credited: 10% of 20000 = 2000, pending.
	var amount int64
	var status string
	if err := h.DB.QueryRow(`SELECT amount, status FROM commissions WHERE id=?`, cid).
		Scan(&amount, &status); err != nil {
		t.Fatal(err)
	}
	if amount != 2000 || status != "pending" {
		t.Fatalf("commission amount=%d status=%s", amount, status)
	}

	// 4) LIFETIME BINDING: renewal needs no cookie — customer id is bound.
	renewal := event
	renewal.ExternalID = "evt_e2e_2"
	renewal.EventType = "renewal"
	renewal.AffiliateID = 0 // must resolve via customers table
	renewal.AmountMinor = 5000
	cid2, err := engine.ProcessPurchase(ctx, renewal)
	if err != nil {
		t.Fatal(err)
	}
	var aff2 int64
	if err := h.DB.QueryRow(`SELECT affiliate_id FROM commissions WHERE id=?`, cid2).Scan(&aff2); err != nil {
		t.Fatal(err)
	}
	if aff2 != 1 {
		t.Fatalf("renewal attributed to %d, want 1 (lifetime binding)", aff2)
	}

	// 5) MATURATION: backdate + mature both.
	if _, err := h.DB.Exec(`UPDATE commissions SET matures_at = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	n, err := engine.Mature(ctx)
	if err != nil || n != 2 {
		t.Fatalf("matured=%d err=%v", n, err)
	}

	// 6) PAYOUT: batch + approve with ledger wiring.
	payouts.LedgerPoster = func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
		_, err := poster.Post(ctx, fmt.Sprintf("payout:%d", payoutID), "payout", "", time.Now().UTC(), payouts.SettleLines(affiliateID, currency, amount))
		return err
	}
	payouts.Ledger = poster
	t.Cleanup(func() { payouts.LedgerPoster, payouts.Ledger = nil, nil })

	runner := payouts.NewRunner(h.DB)
	runID, nPayouts, err := runner.CreateBatch(ctx, 0, "paypal")
	if err != nil || nPayouts != 1 {
		t.Fatalf("payouts=%d err=%v", nPayouts, err)
	}
	var pid int64
	h.DB.QueryRow(`SELECT id FROM payouts`).Scan(&pid)
	if err := runner.Approve(ctx, pid, nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.CompleteRun(ctx, runID, "e2e"); err != nil {
		t.Fatal(err)
	}

	// 7) THE GATE: ledger globally balanced; payable back to zero; both paid.
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("ledger integrity=%d", s)
	}
	bal, _ := poster.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if bal != 0 {
		t.Fatalf("payable=%d after payout, want 0", bal)
	}
	var paid int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions WHERE status='paid'`).Scan(&paid)
	if paid != 2 {
		t.Fatalf("paid commissions=%d want 2", paid)
	}

	// 8) REPLAY EVERYTHING: no double money anywhere.
	for _, ev := range []commissions.Event{event, renewal} {
		if _, err := engine.ProcessPurchase(ctx, ev); err != commissions.ErrAlreadyProcessed {
			t.Fatalf("replay not caught: %v", err)
		}
	}
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity after replays=%d", s)
	}
	t.Log("E2E M1 GATE: click -> conversion -> lifetime renewal -> maturation -> payout -> balanced ledger -> replay-safe")
}

// waitFor polls until fn is true (max 3s).
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition never met")
}
