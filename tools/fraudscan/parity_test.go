// Package fraudscan_test proves the in-browser fraud scan (fraudscan.js)
// and the Go engine agree. Both sides assert testdata/expected.json:
//
//   - affiliate lists: the REAL importer.Parse reads each CSV; its report and
//     normalized affiliates must equal what fraudscan.js produced
//     (testdata/normalized.json), and fraud.DuplicateAccount must flag the
//     same accounts.
//   - conversion lists: the rows fraudscan.js normalized are replayed through
//     the REAL fraud.Engine (dry-run), one commission at a time in the same
//     order, and the per-rule, per-affiliate verdict counts must equal the
//     JS findings.
//
// Run: go test ./tools/fraudscan/  (and node tools/fraudscan/test.mjs)
package fraudscan_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/importer"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

type expectedFixture struct {
	Tool         string                    `json:"tool"`
	ExportKind   string                    `json:"export_kind"`
	RulesRun     []string                  `json:"rules_run"`
	GoFindings   map[string]map[string]int `json:"go_findings"`
	ScanFindings map[string]map[string]int `json:"scan_only_findings"`
}

type normAffiliate struct {
	Email  string `json:"email"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Code   string `json:"code"`
	Status string `json:"status"`
	Coupon string `json:"coupon"`
}

type normConversion struct {
	Row             int    `json:"row"`
	Affiliate       string `json:"affiliate"`
	AffiliateEmail  string `json:"affiliate_email"`
	CustomerEmail   string `json:"customer_email"`
	CustomerKey     string `json:"customer_key"`
	Coupon          string `json:"coupon"`
	CommissionCents *int64 `json:"commission_cents"`
	TS              string `json:"ts"`
	IP              string `json:"ip"`
}

type normFixture struct {
	Kind        string           `json:"kind"`
	Parse       map[string]int   `json:"parse"`
	Affiliates  []normAffiliate  `json:"affiliates"`
	Conversions []normConversion `json:"conversions"`
}

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func newDB(t *testing.T) *db.Handles {
	t.Helper()
	ctx := context.Background()
	h, err := db.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "scan.db"), db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestParity(t *testing.T) {
	var expected map[string]expectedFixture
	var normalized map[string]normFixture
	loadJSON(t, "expected.json", &expected)
	loadJSON(t, "normalized.json", &normalized)
	if len(expected) == 0 || len(expected) != len(normalized) {
		t.Fatalf("expected.json has %d fixtures, normalized.json %d", len(expected), len(normalized))
	}
	names := make([]string, 0, len(expected))
	for n := range expected {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		exp, norm := expected[name], normalized[name]
		t.Run(name, func(t *testing.T) {
			var got map[string]map[string]int
			switch exp.ExportKind {
			case "affiliates":
				got = affiliateList(t, name, exp, norm)
			case "conversions":
				got = replay(t, norm)
			default:
				t.Fatalf("unknown export kind %q", exp.ExportKind)
			}
			want := exp.GoFindings
			if want == nil {
				want = map[string]map[string]int{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go engine verdicts differ from fraudscan.js\n go: %v\n js: %v", got, want)
			}
		})
	}
}

// affiliateList: the real importer parses the CSV; then duplicate_account
// runs over the imported accounts.
func affiliateList(t *testing.T, name string, exp expectedFixture, norm normFixture) map[string]map[string]int {
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	affs, rep, err := importer.Parse(importer.Source(exp.Tool), f)
	if err != nil {
		t.Fatal(err)
	}
	gotParse := map[string]int{"rows": rep.Rows, "affiliates": rep.Affiliates, "skipped": rep.Skipped, "dup_emails": rep.DupEmails}
	if !reflect.DeepEqual(gotParse, norm.Parse) {
		t.Fatalf("importer.Parse report %v, fraudscan.js %v", gotParse, norm.Parse)
	}
	if len(affs) != len(norm.Affiliates) {
		t.Fatalf("importer.Parse %d affiliates, fraudscan.js %d", len(affs), len(norm.Affiliates))
	}
	for i, a := range affs {
		n := norm.Affiliates[i]
		if a.Email != n.Email || a.Name != n.Name || a.Code != n.Code || a.Status != n.Status || a.Coupon != n.Coupon {
			t.Fatalf("affiliate %d: go %+v, js %+v", i, a, n)
		}
	}

	h := newDB(t)
	ctx := context.Background()
	for i, a := range affs {
		if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email, name, status) VALUES (?, ?, ?, ?)`,
			i+1, a.Email, a.Name, a.Status); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]map[string]int{}
	for i, a := range affs {
		vs, err := fraud.DuplicateAccount{DB: h.DB}.Evaluate(ctx, fraud.Query{AffiliateID: int64(i + 1), Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			if got[v.Rule] == nil {
				got[v.Rule] = map[string]int{}
			}
			got[v.Rule][a.Email]++
		}
	}
	return got
}

// replay inserts each conversion as a commission (plus its customer binding
// and, when the export has an IP, a click) and screens it with the full
// default ruleset, exactly as fraudscan.js replays it.
func replay(t *testing.T, norm normFixture) map[string]map[string]int {
	h := newDB(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO journal_entries (id, idem_key, source, effective_at)
		VALUES (1, 'fraudscan-parity', 'conversion', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for i, a := range norm.Affiliates {
		email := a.Email
		if email == "" {
			email = fmt.Sprintf("affiliate-%d@fraudscan.invalid", i+1)
		}
		ids[a.Key] = int64(i + 1)
		if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email, status) VALUES (?, ?, ?)`,
			i+1, email, a.Status); err != nil {
			t.Fatal(err)
		}
	}
	eng := fraud.NewEngine(h.DB)
	bound := map[string]bool{}
	got := map[string]map[string]int{}
	for i, c := range norm.Conversions {
		ts, err := time.Parse(time.RFC3339, c.TS)
		if err != nil {
			t.Fatalf("row %d: timestamp %q: %v", c.Row, c.TS, err)
		}
		ts = ts.UTC()
		aff, ok := ids[c.Affiliate]
		if !ok {
			t.Fatalf("row %d: unknown affiliate %q", c.Row, c.Affiliate)
		}
		// First touch binds the customer (coupon_hijack's customers table).
		if c.CustomerKey != "" && !bound[c.CustomerKey] {
			bound[c.CustomerKey] = true
			if _, err := h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id)
				VALUES (1, ?, ?)`, c.CustomerKey, aff); err != nil {
				t.Fatal(err)
			}
		}
		// amount != 0 is a schema CHECK; a zero or missing commission is
		// still a conversion (fraudscan.js counts it as amount >= 0).
		amount := int64(1)
		if c.CommissionCents != nil && *c.CommissionCents != 0 {
			amount = *c.CommissionCents
		}
		method := "token"
		if c.Coupon != "" {
			method = "coupon"
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions
			(affiliate_id, journal_id, amount, rate_snapshot, status, created_at,
			 buyer_email, attribution_method, coupon_code, customer_ext_id)
			VALUES (?, 1, ?, '{}', 'pending', ?, ?, ?, ?, ?)`,
			aff, amount, ts.Format(time.RFC3339), c.CustomerEmail, method, c.Coupon, c.CustomerKey); err != nil {
			t.Fatal(err)
		}
		var ipHash []byte
		if c.IP != "" {
			sum := sha256.Sum256([]byte(c.IP))
			ipHash = sum[:16]
			day := int64(ts.Year())*10000 + int64(ts.Month())*100 + int64(ts.Day())
			if _, err := h.DB.Exec(`INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id, ip_hash)
				VALUES (?, ?, ?, ?, ?, 1, ?)`,
				fmt.Sprintf("scan-%d", i), ts.Format(time.RFC3339Nano), day, day/100, aff, ipHash); err != nil {
				t.Fatal(err)
			}
		}
		vs, err := eng.Evaluate(ctx, fraud.Query{
			AffiliateID: aff,
			Email:       c.CustomerEmail,
			IPHash:      ipHash,
			CouponCode:  c.Coupon,
			DryRun:      true,
			Now:         ts,
		})
		if err != nil {
			t.Fatalf("row %d: %v", c.Row, err)
		}
		seen := map[string]bool{}
		for _, v := range vs {
			if seen[v.Rule] {
				continue
			}
			seen[v.Rule] = true
			if got[v.Rule] == nil {
				got[v.Rule] = map[string]int{}
			}
			got[v.Rule][c.Affiliate]++
		}
	}
	return got
}

// The JS scan must not skip a rule the Go engine can run on the same
// inputs without saying why: every Go rule name appears in expected.json
// either as run or skipped (the JS test enforces the reasons).
func TestParity_RuleNamesMatchEngine(t *testing.T) {
	var expected map[string]struct {
		RulesRun     []string `json:"rules_run"`
		RulesSkipped []string `json:"rules_skipped"`
	}
	loadJSON(t, "expected.json", &expected)
	h := newDB(t)
	var engine []string
	for _, r := range fraud.NewEngine(h.DB).Rules() {
		engine = append(engine, r.Name())
	}
	sort.Strings(engine)
	scanOnly := map[string]bool{"refund_rate": true, "click_to_conversion": true}
	for name, e := range expected {
		var all []string
		for _, r := range append(append([]string{}, e.RulesRun...), e.RulesSkipped...) {
			if !scanOnly[r] {
				all = append(all, r)
			}
		}
		sort.Strings(all)
		if strings.Join(all, ",") != strings.Join(engine, ",") {
			t.Fatalf("%s: scan covers %v, engine has %v", name, all, engine)
		}
	}
}
