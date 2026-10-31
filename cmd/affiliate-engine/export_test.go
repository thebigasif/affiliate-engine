package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// The export subcommand against a real (migrated, seeded) SQLite
// database: the full operator path — export, count report, -verify
// round-trip. (The PG leg of the library runs via internal/analytics;
// dbtest does not expose a reopenable DSN for PG.)

func exportTestDB(t *testing.T) string {
	t.Helper()
	if dbtest.IsPostgres() {
		t.Skip("PG DSN reconstruction is not exposed by dbtest; the PG leg runs via internal/analytics tests")
	}
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "export.db")

	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(context.Background(), h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'e@t.dev')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ck1", "ck2", "ck3"} {
		if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id)
			VALUES (?, '2026-09-01T00:00:00Z', 20260901, 202609, 1, 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.Exec(`INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents)
		VALUES (20260901, 1, 1, '', 42, 0, 0), (20260901, 1, 1, 'USD', 0, 2, 500)`); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestExportParquet_Run(t *testing.T) {
	dsn := exportTestDB(t)
	outDir := filepath.Join(t.TempDir(), "export")

	out := captureStdout(t, func() {
		if err := runExportParquet(dsn, []string{"-out", outDir, "-verify"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "exported 3 clicks") || !strings.Contains(out, "exported 2 daily_agg rows") {
		t.Fatalf("count report wrong:\n%s", out)
	}
	if !strings.Contains(out, "verify OK") {
		t.Fatalf("verify did not pass:\n%s", out)
	}
	for _, f := range []string{"clicks.parquet", "daily_agg.parquet"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
}

// Day-bound flags validate (real calendar days only) and filter.
func TestExportParquet_DayFlagValidation(t *testing.T) {
	dsn := exportTestDB(t)
	for _, b := range []string{"2026091", "2026-09-01", "20261332", "abc12345", "20260230"} {
		if err := runExportParquet(dsn, []string{"-from", b}); err == nil {
			t.Fatalf("-from %q must be rejected", b)
		}
	}
	outDir := filepath.Join(t.TempDir(), "d")
	out := captureStdout(t, func() {
		if err := runExportParquet(dsn, []string{"-out", outDir, "-from", "20260901", "-to", "20260901", "-verify"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "exported 3 clicks") {
		t.Fatalf("bounded export wrong:\n%s", out)
	}
}
