package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// testconvEnv builds a migrated file-backed DB for CLI drill tests.
func testconvEnv(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drill.db")
	h, err := db.Open(ctx, "sqlite:"+path, db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	// The operator's minimal setup: one affiliate (portal-apply approved),
	// one 10% rule. Payable account comes from the affiliates trigger.
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'drill@aff.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO commission_rules (campaign_id, affiliate_id, rate_type, rate_value)
		VALUES (1, NULL, 'percent', 1000)`); err != nil {
		t.Fatal(err)
	}
	return path
}

// No -affiliate: actionable error, nothing written.
func TestTestConversion_RequiresAffiliate(t *testing.T) {
	dbPath := testconvEnv(t)
	err := runTestConversion(dbPath, []string{})
	if err == nil || !strings.Contains(err.Error(), "affiliate") {
		t.Fatalf("err=%v want affiliate guidance", err)
	}
}

// Unknown affiliate: points at the apply+review flow.
func TestTestConversion_UnknownAffiliate(t *testing.T) {
	dbPath := testconvEnv(t)
	err := runTestConversion(dbPath, []string{"-affiliate", "99"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err=%v want not-found guidance", err)
	}
}

// Missing rule: the error tells the operator the exact POST to make.
func TestTestConversion_NoRule(t *testing.T) {
	dbPath := testconvEnv(t)
	err := runTestConversion(dbPath, []string{"-affiliate", "1", "-campaign", "7"})
	if err == nil || !strings.Contains(err.Error(), "commission rule") {
		t.Fatalf("err=%v want rule guidance", err)
	}
}

// Happy path: commission credited through the real engine, ledger balanced,
// rate applied (10% of 5000 = 500), raw_events row marked processed.
func TestTestConversion_Credits(t *testing.T) {
	dbPath := testconvEnv(t)
	if err := runTestConversion(dbPath, []string{"-affiliate", "1", "-amount", "5000", "-external-id", "drill-1"}); err != nil {
		t.Fatal(err)
	}
	h, err := db.Open(context.Background(), "sqlite:"+dbPath, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.DB.Close()
	var amount int64
	var status, source string
	if err := h.DB.QueryRow(`
		SELECT c.amount, c.status, re.source FROM commissions c
		JOIN raw_events re ON re.external_event_id = 'drill-1'
		WHERE c.id = (SELECT MAX(id) FROM commissions)`).Scan(&amount, &status, &source); err != nil {
		t.Fatal(err)
	}
	if amount != 500 || status != "pending" || source != "test" {
		t.Fatalf("amount=%d status=%s source=%s", amount, status, source)
	}
	var sum int64
	h.DB.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&sum)
	if sum != 0 {
		t.Fatalf("ledger sum=%d", sum)
	}
}

// Replay the same external-id: rejected exactly like production.
func TestTestConversion_ReplayRejected(t *testing.T) {
	dbPath := testconvEnv(t)
	if err := runTestConversion(dbPath, []string{"-affiliate", "1", "-external-id", "drill-r"}); err != nil {
		t.Fatal(err)
	}
	err := runTestConversion(dbPath, []string{"-affiliate", "1", "-external-id", "drill-r"})
	if err == nil || !strings.Contains(err.Error(), "already processed") {
		t.Fatalf("err=%v want replay rejection", err)
	}
	h, err := db.Open(context.Background(), "sqlite:"+dbPath, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.DB.Close()
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("commissions=%d after replay want 1", n)
	}
}
