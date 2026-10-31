package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// The import subcommand end to end on a SQLite file: dry-run writes
// nothing, a real run prints the next steps for the incumbent's link
// param, JSON mode stays machine-readable.

func importTestDB(t *testing.T) (dsn, csvPath string) {
	t.Helper()
	if dbtest.IsPostgres() {
		t.Skip("the CLI leg runs on SQLite (dbtest exposes no reopenable PG DSN)")
	}
	dir := t.TempDir()
	csvPath = filepath.Join(dir, "export.csv")
	if err := os.WriteFile(csvPath, []byte("email,name,referral_id,status,coupon\n"+
		"jane@shop.io,Jane,janeslink,active,JANE20\n"+
		"bob@shop.io,Bob,bobby,active,\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "sqlite:" + filepath.Join(dir, "imp.db"), csvPath
}

func tableCounts(t *testing.T, dsn string) string {
	t.Helper()
	h, err := db.Open(context.Background(), dsn, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Ledger.Close(); _ = h.DB.Close() }()
	var parts []string
	for _, tbl := range []string{"affiliates", "affiliate_codes", "coupons", "import_batches", "import_undos", "ledger_accounts"} {
		var n int
		if err := h.DB.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", tbl, n))
	}
	return strings.Join(parts, " ")
}

func TestImport_DryRunWritesNothing(t *testing.T) {
	dsn, csvPath := importTestDB(t)
	// First pass migrates the fresh db, so the counts compare like with like.
	captureStdout(t, func() {
		if err := runImport(dsn, []string{"-from", "rewardful", "-csv", csvPath, "--dry-run"}); err != nil {
			t.Fatal(err)
		}
	})
	before := tableCounts(t, dsn)
	out := captureStdout(t, func() {
		if err := runImport(dsn, []string{"-from", "rewardful", "-csv", csvPath, "--dry-run"}); err != nil {
			t.Fatal(err)
		}
	})
	if after := tableCounts(t, dsn); after != before {
		t.Fatalf("dry-run wrote rows: %s -> %s", before, after)
	}
	if !strings.Contains(out, "dry-run (nothing written)") || !strings.Contains(out, "codes_created=2") {
		t.Fatalf("dry-run output: %s", out)
	}
	if strings.Contains(out, "Next steps") {
		t.Fatalf("dry-run printed next steps: %s", out)
	}
}

func TestImport_NextStepsNameTheLinkParam(t *testing.T) {
	for src, param := range map[string]string{"rewardful": "via", "firstpromoter": "fpr", "tolt": "ref", "tapfiliate": "ref"} {
		t.Run(src, func(t *testing.T) {
			dsn, csvPath := importTestDB(t)
			t.Setenv("AE_PUBLIC_URL", "https://acme.affiliatetracking.co/")
			out := captureStdout(t, func() {
				if err := runImport(dsn, []string{"-from", src, "-csv", csvPath}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "codes_created=2") {
				t.Fatalf("summary: %s", out)
			}
			snippet := `<script src="https://acme.affiliatetracking.co/t.js" data-param="` + param + `" async></script>`
			if !strings.Contains(out, snippet) || !strings.Contains(out, "?"+param+"=CODE") {
				t.Fatalf("next steps missing %s snippet:\n%s", param, out)
			}
		})
	}
}

func TestImport_NextStepsWithoutPublicURL(t *testing.T) {
	dsn, csvPath := importTestDB(t)
	t.Setenv("AE_PUBLIC_URL", "javascript:alert(1)")
	out := captureStdout(t, func() {
		if err := runImport(dsn, []string{"-from", "rewardful", "-csv", csvPath}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `<script src="https://YOUR-ENGINE-HOST/t.js" data-param="via" async></script>`) {
		t.Fatalf("placeholder snippet missing:\n%s", out)
	}
}

func TestImport_JSONStaysMachineReadable(t *testing.T) {
	dsn, csvPath := importTestDB(t)
	out := captureStdout(t, func() {
		if err := runImport(dsn, []string{"-from", "rewardful", "-csv", csvPath, "-json"}); err != nil {
			t.Fatal(err)
		}
	})
	var rep struct {
		BatchID int64 `json:"batch_id"`
		Codes   int   `json:"codes"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.BatchID == 0 || rep.Codes != 2 {
		t.Fatalf("json output %q: %+v %v", out, rep, err)
	}
}

// writeFile drops a CSV next to the test db.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The switch flow end to end through the CLI: review (nothing written),
// import, re-run (nothing new), list, undo, undo again.
func TestImport_SwitchFlowCLI(t *testing.T) {
	dsn, csvPath := importTestDB(t)
	dir := filepath.Dir(csvPath)
	refs := writeFile(t, dir, "refs.csv", "id,affiliate_email,customer_email,stripe_customer_id,status,created_at\n"+
		"r1,jane@shop.io,jane+alt@shop.io,cus_1,conversion,2026-09-20T10:00:00Z\n"+
		"r2,bob@shop.io,c2@x.io,cus_2,conversion,2026-09-21T10:00:00Z\n")
	coms := writeFile(t, dir, "coms.csv", "id,affiliate_email,amount,currency,state,created_at\n"+
		"c1,jane@shop.io,10.00,USD,paid,2026-09-20T10:00:00Z\n"+
		"c2,bob@shop.io,5,ABC,due,2026-09-21T10:00:00Z\n")
	args := []string{"-from", "rewardful", "-csv", csvPath, "-referrals", refs, "-commissions", coms}

	captureStdout(t, func() { _ = runImport(dsn, []string{"-list"}) }) // migrate the fresh db first
	before := tableCounts(t, dsn)
	out := captureStdout(t, func() {
		if err := runImport(dsn, append(args, "-dry-run")); err != nil {
			t.Fatal(err)
		}
	})
	if after := tableCounts(t, dsn); after != before {
		t.Fatalf("review wrote rows: %s -> %s", before, after)
	}
	for _, want := range []string{
		"dry-run (nothing written)", "2 customer bindings would be created",
		"unknown_currency", "could not run  click_velocity", "self_referral  jane@shop.io", "USD 10.00 total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("review output missing %q:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() {
		if err := runImport(dsn, args); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "batch_id=1") || !strings.Contains(out, "import -undo 1") {
		t.Fatalf("import output:\n%s", out)
	}
	afterImport := tableCounts(t, dsn)
	out = captureStdout(t, func() {
		if err := runImport(dsn, args); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "already imported as batch 1") || tableCounts(t, dsn) != afterImport {
		t.Fatalf("re-run output:\n%s", out)
	}
	out = captureStdout(t, func() {
		if err := runImport(dsn, []string{"-undo", "1"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "undone batch 1 (rewardful): removed affiliates=2 codes=2 coupons=1 customer_bindings=2") {
		t.Fatalf("undo output:\n%s", out)
	}
	out = captureStdout(t, func() {
		if err := runImport(dsn, []string{"-undo", "1"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "already undone") {
		t.Fatalf("second undo output:\n%s", out)
	}
	out = captureStdout(t, func() { _ = runImport(dsn, []string{"-list"}) })
	if !strings.Contains(out, "rewardful") || strings.Contains(out, "  -\n") {
		t.Fatalf("list output:\n%s", out)
	}
}

func TestCompare_CLI(t *testing.T) {
	dsn, csvPath := importTestDB(t)
	dir := filepath.Dir(csvPath)
	coms := writeFile(t, dir, "coms.csv", "id,affiliate_email,amount,currency,state,created_at\n"+
		"c1,jane@shop.io,10.00,USD,paid,2026-09-20T10:00:00Z\n"+
		"c2,jane@shop.io,99,USD,paid,2026-09-25T10:00:00Z\n")
	out := captureStdout(t, func() {
		if err := runCompare(dsn, []string{"-from", "rewardful", "-commissions", coms, "-date", "2026-09-20"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"Side by side: rewardful vs AffiliateTracking, 2026-09-20", "commission amount USD  10.00", "-10.00", "clicks: no click export"} {
		if !strings.Contains(out, want) {
			t.Fatalf("compare output missing %q:\n%s", want, out)
		}
	}
	if err := runCompare(dsn, []string{"-from", "rewardful"}); err == nil {
		t.Fatal("compare without any export accepted")
	}
	if err := runCompare(dsn, []string{"-from", "rewardful", "-commissions", coms, "-date", "yesterday"}); err == nil {
		t.Fatal("bad date accepted")
	}
}
