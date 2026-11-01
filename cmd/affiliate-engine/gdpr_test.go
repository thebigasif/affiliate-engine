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
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
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

// The CLI export opens the affiliate's payout details with the
// deployment secret, and leaves them sealed when it has none.
func TestGDPR_ExportCLIPayoutDetails(t *testing.T) {
	dsn := gdprTestDB(t)
	secret := strings.Repeat("ab", 32)
	t.Setenv("AE_TOKEN_SECRET", secret)
	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Affiliate 1 is the first active one: the demo seeder gives it
	// PayPal with its own email.
	if err := httpapi.SeedDemoPayoutDetails(context.Background(), h.DB, httpapi.DeriveTaxDocKey([]byte(secret))); err != nil {
		t.Fatal(err)
	}
	_ = h.Ledger.Close()
	_ = h.DB.Close()

	export := func() map[string]any {
		t.Helper()
		out := filepath.Join(t.TempDir(), "subject-1.json")
		captureStdout(t, func() {
			if err := runGDPRExport(dsn, []string{"-affiliate", "1", "-out", out}); err != nil {
				t.Fatal(err)
			}
		})
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var b struct {
			PayoutDetails map[string]any `json:"payout_details"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		return b.PayoutDetails
	}
	pd := export()
	if pd["paypal_email"] != "subj@t.dev" || pd["details_readable"] != true {
		t.Fatalf("payout details = %v", pd)
	}

	// No secret anywhere: listed, not opened, and no secret is created.
	t.Setenv("AE_TOKEN_SECRET", "")
	t.Setenv("AE_SECRET_FILE", filepath.Join(t.TempDir(), "missing.secret"))
	pd = export()
	if pd["details_readable"] != false || pd["paypal_email"] != nil {
		t.Fatalf("payout details without secret = %v", pd)
	}
	if _, err := os.Stat(os.Getenv("AE_SECRET_FILE")); !os.IsNotExist(err) {
		t.Fatalf("export created a secret file: %v", err)
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

// The buyer commands: export finds the buyer's stored event, erase needs
// -yes, redacts it, and a re-run reports that nothing changed.
func TestGDPR_BuyerCLI(t *testing.T) {
	dsn := gdprTestDB(t)
	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO raw_events (source, external_event_id, event_type, payload)
		VALUES ('stripe', 'evt_1', 'purchase', ?)`,
		`{"ExternalID":"evt_1","AmountMinor":1000,"CustomerEmail":"buyer@t.dev","CustomerExtID":"cus_9","AffiliateID":1}`); err != nil {
		t.Fatal(err)
	}
	_ = h.Ledger.Close()
	_ = h.DB.Close()

	if err := runGDPRExportBuyer(dsn, nil); err == nil || !strings.Contains(err.Error(), "-email or -customer") {
		t.Fatalf("missing selector: %v", err)
	}
	out := filepath.Join(t.TempDir(), "buyer.json")
	captureStdout(t, func() {
		if err := runGDPRExportBuyer(dsn, []string{"-email", "buyer@t.dev", "-out", out}); err != nil {
			t.Fatal(err)
		}
	})
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(raw, &b); err != nil || len(b.Events) != 1 || b.Events[0]["external_event_id"] != "evt_1" {
		t.Fatalf("buyer bundle events = %v (%v)", b.Events, err)
	}

	if err := runGDPREraseBuyer(dsn, []string{"-customer", "cus_9"}); err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("missing -yes guard: %v", err)
	}
	stdout := captureStdout(t, func() {
		if err := runGDPREraseBuyer(dsn, []string{"-customer", "cus_9", "-yes", "-json"}); err != nil {
			t.Fatal(err)
		}
	})
	var rep struct {
		Scrubbed map[string]int64 `json:"scrubbed"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || rep.Scrubbed["raw_events"] != 1 {
		t.Fatalf("erase report = %s (%v)", stdout, err)
	}
	stdout = captureStdout(t, func() {
		if err := runGDPREraseBuyer(dsn, []string{"-email", "buyer@t.dev", "-yes"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, "nothing changed") {
		t.Fatalf("re-run output: %s", stdout)
	}
	if err := runGDPRExportBuyer(dsn, []string{"-email", "buyer@t.dev"}); err == nil || !strings.Contains(err.Error(), "no stored data") {
		t.Fatalf("export after erase: %v", err)
	}
}

// Erase refuses to run, changing nothing, when payout details exist but
// no secret opens them; -allow-sealed-payout erases and says what stayed.
func TestGDPR_EraseCLISealedPayout(t *testing.T) {
	dsn := gdprTestDB(t)
	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := httpapi.SeedDemoPayoutDetails(context.Background(), h.DB, httpapi.DeriveTaxDocKey([]byte(strings.Repeat("cd", 32)))); err != nil {
		t.Fatal(err)
	}
	_ = h.Ledger.Close()
	_ = h.DB.Close()
	t.Setenv("AE_TOKEN_SECRET", "")
	t.Setenv("AE_SECRET_FILE", filepath.Join(t.TempDir(), "missing.secret"))

	var runErr error
	captureStdout(t, func() { runErr = runGDPRErase(dsn, []string{"-affiliate", "1", "-yes"}) })
	if runErr == nil || !strings.Contains(runErr.Error(), "-allow-sealed-payout") {
		t.Fatalf("sealed payout guard: %v", runErr)
	}
	stdout := captureStdout(t, func() {
		if err := runGDPRErase(dsn, []string{"-affiliate", "1", "-yes", "-allow-sealed-payout"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, "NOT redacted in stored events: payout identifiers") {
		t.Fatalf("report does not say payout identifiers stayed:\n%s", stdout)
	}
}
