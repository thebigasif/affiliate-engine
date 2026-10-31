package e2e

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// The fresh-install invariant (M1 gate): after migrations on an empty
// database, the money path works with ZERO manual seeding. Before the
// accounts_* migrations this failed with `ledger: account
// "EXPENSE:COMMISSION": no rows` — tests had been papering over the gap by
// calling EnsureAccounts themselves. Accounts are per currency now and the
// ledger creates them on first use.
func TestFreshInstall_FirstCommission(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}

	// No accounts are seeded: they are per currency, created on first use.
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM ledger_accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fresh install seeded %d ledger accounts, want 0 (per-currency accounts are created on first use)", n)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'fresh@install.dev')`); err != nil {
		t.Fatal(err)
	}

	// A rule + a purchase — the full credit path, no EnsureAccounts anywhere.
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value)
		VALUES (1, NULL, 'percent', 1000)`); err != nil {
		t.Fatal(err)
	}
	poster := ledger.NewPoster(h.Ledger)
	engine := commissions.NewEngine(h.DB, poster)
	if _, err := engine.ProcessPurchase(ctx, commissions.Event{
		Source: "test", ExternalID: "fresh-1", EventType: "purchase",
		AmountMinor: 10000, Currency: "USD",
		AffiliateID: 1, CampaignID: 1, OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("first commission on fresh install: %v (the M1 gate)", err)
	}

	// Maturation moves HOLD -> payable without manual accounts either.
	if _, err := h.DB.Exec(`UPDATE commissions SET matures_at = ?`, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := engine.Mature(ctx); err != nil || n != 1 {
		t.Fatalf("mature: n=%d err=%v", n, err)
	}
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
	bal, err := poster.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil || bal != -1000 {
		t.Fatalf("payable balance=%d err=%v want -1000", bal, err)
	}

	// Replay rejected, ledger untouched.
	if _, err := engine.ProcessPurchase(ctx, commissions.Event{
		Source: "test", ExternalID: "fresh-1", EventType: "purchase",
		AmountMinor: 10000, Currency: "USD",
		AffiliateID: 1, CampaignID: 1, OccurredAt: time.Now().UTC(),
	}); err != commissions.ErrAlreadyProcessed {
		t.Fatalf("replay: %v", err)
	}

	// Re-running migrations is a no-op on accounts (idempotency).
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var total int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM ledger_accounts`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 { // HOLD:USD, EXPENSE:COMMISSION:USD, AFFILIATE_PAYABLE:1:USD
		t.Fatalf("accounts after re-migrate=%d want 3", total)
	}
}
