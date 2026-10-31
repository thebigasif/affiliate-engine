package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// AE_FRAUD_DRYRUN must hold on the periodic sweep too: a dry-run sweep
// records only dry-run verdicts, an enforcing sweep records enforcing
// ones. Neither places holds: the sweep evaluates affiliates, not a
// commission, and fraud holds are scoped to the evaluated commission and
// click-linked commissions (a sweep verdict must never blanket-hold an
// affiliate's whole balance; see internal/fraud engine.go "Hold scope").
func TestFraudSweep_HonoursDryRun(t *testing.T) {
	ctx := context.Background()
	h, err := db.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "sweep.db"), db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'sweep@t.dev')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at) VALUES ('sw', 'test', ?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// 30 commissions in the last hour trips conversion_velocity.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < 30; i++ {
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
			VALUES (1, (SELECT id FROM journal_entries WHERE idem_key='sw'), 100, '{}', 'pending', ?)`, now); err != nil {
			t.Fatal(err)
		}
	}
	fe := fraud.NewEngine(h.DB)
	count := func(q string) int {
		t.Helper()
		var n int
		if err := h.DB.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	fraudSweep(ctx, fe, h.DB, true)
	if count(`SELECT COUNT(*) FROM fraud_verdicts WHERE dry_run = TRUE`) == 0 {
		t.Fatal("dry-run sweep recorded no verdicts")
	}
	if n := count(`SELECT COUNT(*) FROM fraud_verdicts WHERE dry_run = FALSE`); n != 0 {
		t.Fatalf("dry-run sweep recorded %d enforcing verdicts", n)
	}
	if n := count(`SELECT COUNT(*) FROM commission_holds`); n != 0 {
		t.Fatalf("dry-run sweep placed %d holds (must not enforce)", n)
	}

	fraudSweep(ctx, fe, h.DB, false)
	if count(`SELECT COUNT(*) FROM fraud_verdicts WHERE dry_run = FALSE`) == 0 {
		t.Fatal("enforcing sweep recorded no enforcing verdicts")
	}
	if n := count(`SELECT COUNT(*) FROM commission_holds`); n != 0 {
		t.Fatalf("enforcing sweep blanket-held %d commissions (holds are scoped to the evaluated commission)", n)
	}
}
