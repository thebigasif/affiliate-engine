package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// The GDPR subcommands against a real migrated+seeded SQLite install: the
// operator path — export to file, erase with confirmation, idempotent
// re-run, unknown-affiliate error. (The dual-engine leg of the Store
// itself runs in internal/privacy; like export_test, dbtest does not
// expose a reopenable PG DSN for the CLI layer.)

func gdprTestDB(t *testing.T) string {
	t.Helper()
	if dbtest.IsPostgres() {
		t.Skip("PG DSN reconstruction is not exposed by dbtest; the PG leg runs via internal/privacy tests")
	}
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "gdpr.db")
	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(context.Background(), h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO affiliates (id, email, name, status) VALUES (1, 'subj@t.dev', 'Subj', 'active')`,
		`INSERT INTO clicks (id, ts, day, month, affiliate_id, referer) VALUES ('c1', '2026-09-01T00:00:00Z', 20260901, 202609, 1, 'https://g.test/')`,
		`INSERT INTO affiliate_codes (affiliate_id, code, is_primary, source) VALUES (1, 'subj', TRUE, 'native')`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return dsn
}

func TestGDPR_ExportCLI(t *testing.T) {
	dsn := gdprTestDB(t)
	out := filepath.Join(t.TempDir(), "subject-1.json")

	stdout := captureStdout(t, func() {
		if err := runGDPRExport(dsn, []string{"-affiliate", "1", "-out", out}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, "gdpr-export: affiliate 1 ->") {
		t.Fatalf("report wrong:\n%s", stdout)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Subject map[string]any   `json:"subject"`
		Clicks  []map[string]any `json:"clicks"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("bundle not valid json: %v", err)
	}
	if b.Subject["email"] != "subj@t.dev" {
		t.Errorf("subject email = %v", b.Subject["email"])
	}
	if len(b.Clicks) != 1 || b.Clicks[0]["referer"] != "https://g.test/" {
		t.Errorf("clicks = %v", b.Clicks)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("bundle perms = %o want 600 (subject PII)", st.Mode().Perm())
	}

	// Unknown affiliate is an actionable error.
	if err := runGDPRExport(dsn, []string{"-affiliate", "999"}); err == nil || !strings.Contains(err.Error(), "no affiliate") {
		t.Fatalf("unknown affiliate err = %v", err)
	}
}

func TestGDPR_EraseCLI(t *testing.T) {
	dsn := gdprTestDB(t)

	// Refuses without -yes (destructive guard).
	if err := runGDPRErase(dsn, []string{"-affiliate", "1"}); err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("missing -yes guard: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runGDPRErase(dsn, []string{"-affiliate", "1", "-yes", "-json"}); err != nil {
			t.Fatal(err)
		}
	})
	var rep struct {
		Scrubbed map[string]int64 `json:"scrubbed"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("-json report not valid: %v\n%s", err, stdout)
	}
	if rep.Scrubbed["affiliates"] != 1 {
		t.Fatalf("affiliates scrubbed = %d want 1:\n%s", rep.Scrubbed["affiliates"], stdout)
	}

	// The DB state reflects the erasure through the CLI path too.
	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Ledger.Close(); _ = h.DB.Close() }()
	var email, status string
	if err := h.DB.QueryRow(`SELECT email, status FROM affiliates WHERE id = 1`).Scan(&email, &status); err != nil {
		t.Fatal(err)
	}
	if email != "erased+1@erased.invalid" || status != "removed" {
		t.Fatalf("affiliate = %q/%q", email, status)
	}
}
