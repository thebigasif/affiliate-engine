package importer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
	"github.com/thebigasif/affiliate-engine/internal/importer"
	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

func newEnv(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	return h
}

// Realistic export shapes per incumbent (headers reconstructed from their
// public CSV exports / API docs; each exercises a different header style).
var fixtures = map[string]struct {
	src importer.Source
	csv string
}{
	"rewardful": {
		importer.Rewardful,
		"email,name,referral_id,status,coupon\n" +
			"jane@shop.io,Jane Doe,janeslink,active,JANE20\n" +
			"bob@shop.io,Bob,janeslink,active,\n" + // code clash with jane
			"banned@shop.io,Bad,person,blocked,\n",
	},
	"firstpromoter": {
		importer.FirstPromoter,
		"Email Address,Full Name,Referral ID,State,Coupon Code\n" +
			"one@saas.dev,One PM,ref_one,accepted,SAVE10\n" +
			"two@saas.dev,Two,ref_two,pending,\n" +
			"not-an-email,Bad,,,\n",
	},
	"tapfiliate": {
		importer.Tapfiliate,
		"email,ref_id,status\n" +
			"tap@example.com,tapslug,approved\n",
	},
	"tolt": {
		importer.Tolt,
		"email,name,handle,status,coupons\n" +
			"t@tolt.app,T User,tolt-handle,enabled,TOLT5\n" +
			"t@tolt.app,Dup Email,tolt-handle,enabled,\n", // in-file dup email
	},
}

// IM1: every incumbent parses to normalized affiliates.
func TestParse_AllIncumbents(t *testing.T) {
	for name, fx := range fixtures {
		affs, rep, err := importer.Parse(fx.src, strings.NewReader(fx.csv))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.Affiliates == 0 {
			t.Fatalf("%s: parsed zero affiliates", name)
		}
		for _, a := range affs {
			if a.Email == "" || !strings.Contains(a.Email, "@") {
				t.Fatalf("%s: bad email %q", name, a.Email)
			}
			if a.Status != "active" && a.Status != "frozen" {
				t.Fatalf("%s: bad status %q", name, a.Status)
			}
		}
	}
	// Spot checks.
	affs, rep, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))
	if rep.Skipped != 0 || rep.Affiliates != 3 {
		t.Fatalf("rewardful: affs=%d skipped=%d", rep.Affiliates, rep.Skipped)
	}
	if affs[2].Status != "frozen" {
		t.Fatalf("blocked must map to frozen, got %q", affs[2].Status)
	}
	_, rep, _ = importer.Parse(importer.FirstPromoter, strings.NewReader(fixtures["firstpromoter"].csv))
	if rep.Skipped != 1 {
		t.Fatalf("firstpromoter: skipped=%d, want 1 (bad email)", rep.Skipped)
	}
	_, rep, _ = importer.Parse(importer.Tolt, strings.NewReader(fixtures["tolt"].csv))
	if rep.DupEmails != 1 {
		t.Fatalf("tolt: dups=%d, want 1", rep.DupEmails)
	}
}

// IM2: unrecognized header rejected.
func TestParse_RejectsUnknownHeader(t *testing.T) {
	if _, _, err := importer.Parse(importer.Rewardful, strings.NewReader("a,b,c\n1,2,3\n")); err == nil {
		t.Fatal("garbage header accepted")
	}
}

// IM3: dry-run writes nothing but reports everything.
func TestMigrate_DryRun(t *testing.T) {
	h := newEnv(t)
	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))
	rep, err := importer.Migrate(context.Background(), h.DB, importer.Rewardful, affs, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DryRun != true || rep.BatchID != 0 {
		t.Fatalf("dry-run report: %+v", rep)
	}
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliates`).Scan(&n)
	if n != 0 {
		t.Fatalf("dry-run wrote %d affiliates", n)
	}
	h.DB.QueryRow(`SELECT COUNT(*) FROM import_batches`).Scan(&n)
	if n != 0 {
		t.Fatalf("dry-run wrote a batch row")
	}
}

// IM3b: dry-run warning semantics — a fresh DB warns on nothing; a DB that
// already holds the affiliate warns only for the pre-existing email (the
// real run would attach codes/coupons and leave the row untouched). The old
// `dryRun && !created` condition warned on EVERY row because `created` is
// always false in dry-run; `dryRun && exists` is the correct discriminator.
func TestMigrate_DryRunWarnings(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))

	// Fresh DB: no warnings — everything would be created.
	rep, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) != 0 {
		t.Fatalf("fresh dry-run warnings=%v, want none", rep.Warnings)
	}

	// Real import, then dry-run again: the pre-existing emails warn.
	if _, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false); err != nil {
		t.Fatal(err)
	}
	rep, err = importer.Migrate(ctx, h.DB, importer.Rewardful, affs, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) != 3 {
		t.Fatalf("re-import dry-run warnings=%d (%v), want 3 (one per existing email)", len(rep.Warnings), rep.Warnings)
	}
	for _, w := range rep.Warnings {
		if !strings.Contains(w, "exists") {
			t.Fatalf("warning %q must flag a pre-existing email", w)
		}
	}
}

// IM4: real migration — slug preservation, clash suffixing, coupon import,
// idempotent re-import, rollback export.
func TestMigrate_FullLifecycle(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))

	rep, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BatchID == 0 {
		t.Fatal("no batch recorded")
	}
	if rep.CodeClashes != 1 {
		t.Fatalf("clashes=%d, want 1 (bob's janeslink suffixes)", rep.CodeClashes)
	}

	// Slug preservation: jane's code landed as-is, bob got the -2 suffix.
	var idJane, idBob int64
	h.DB.QueryRow(`SELECT affiliate_id FROM affiliate_codes WHERE code='janeslink' AND is_primary=TRUE`).Scan(&idJane)
	h.DB.QueryRow(`SELECT affiliate_id FROM affiliate_codes WHERE code='janeslink-2'`).Scan(&idBob)
	if idJane == 0 || idBob == 0 || idJane == idBob {
		t.Fatalf("codes wrong: jane=%d bob=%d", idJane, idBob)
	}

	// /r/janeslink resolves jane through the real HTTP path.
	mux := http.NewServeMux()
	_ = mux // resolution tested at DB level below (handler covered elsewhere)
	var status string
	h.DB.QueryRow(`SELECT status FROM affiliates WHERE id=?`, idJane).Scan(&status)
	if status != "active" {
		t.Fatalf("jane status=%q", status)
	}
	var frozen string
	h.DB.QueryRow(`SELECT status FROM affiliates WHERE email='banned@shop.io'`).Scan(&frozen)
	if frozen != "frozen" {
		t.Fatalf("blocked affiliate must import frozen, got %q", frozen)
	}

	// Coupon imported.
	var cn int
	h.DB.QueryRow(`SELECT COUNT(*) FROM coupons WHERE code='jane20'`).Scan(&cn)
	if cn != 1 {
		t.Fatalf("coupon not imported (lowercased)")
	}

	// Re-import is a no-op (idempotent).
	_, err = importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false)
	if err != nil {
		t.Fatal(err)
	}
	var affCount int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliates`).Scan(&affCount)
	if affCount != 3 {
		t.Fatalf("re-import duplicated affiliates: %d", affCount)
	}

	// Rollback export contains the created affiliates + codes.
	var sb strings.Builder
	if err := importer.RollbackExport(ctx, h.DB, rep.BatchID, &sb); err != nil {
		t.Fatal(err)
	}
	var undo struct {
		Source     string `json:"source"`
		Affiliates []struct {
			AffiliateID int64  `json:"affiliate_id"`
			Email       string `json:"email"`
			Code        string `json:"code"`
			Created     bool   `json:"created"`
		} `json:"affiliates"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &undo); err != nil {
		t.Fatalf("undo json: %v", err)
	}
	if undo.Source != "rewardful" || len(undo.Affiliates) != 3 {
		t.Fatalf("undo payload: %+v", undo)
	}

	// Unknown batch: clean error.
	if err := importer.RollbackExport(ctx, h.DB, 999, &sb); err == nil {
		t.Fatal("unknown batch accepted")
	}
}

// IM5: cross-source import — codes from two incumbents never collide.
func TestMigrate_CrossSource(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	for _, name := range []string{"rewardful", "firstpromoter", "tapfiliate", "tolt"} {
		fx := fixtures[name]
		affs, _, err := importer.Parse(fx.src, strings.NewReader(fx.csv))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := importer.Migrate(ctx, h.DB, fx.src, affs, false); err != nil {
			t.Fatalf("%s migrate: %v", name, err)
		}
	}
	var codes int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_codes`).Scan(&codes)
	if codes == 0 {
		t.Fatal("no codes imported")
	}
	// UNIQUE(code) held: no error across four sources.
	var dupes int
	h.DB.QueryRow(`SELECT COUNT(*) FROM (SELECT code FROM affiliate_codes GROUP BY code HAVING COUNT(*) > 1)`).Scan(&dupes)
	if dupes != 0 {
		t.Fatalf("duplicate codes: %d", dupes)
	}
}

// IM6: a switcher's published links keep working the moment the import
// lands: /r/<imported code> records a click for the right affiliate and
// hands a token for that affiliate to the destination; a code suffixed on
// a clash belongs to the second affiliate; a frozen import tracks nothing.
func TestMigrate_ImportedCodesRedirect(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))
	if _, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false); err != nil {
		t.Fatal(err)
	}
	idOf := func(email string) int64 {
		var id int64
		if err := h.DB.QueryRow(`SELECT id FROM affiliates WHERE email = ?`, email).Scan(&id); err != nil {
			t.Fatalf("%s: %v", email, err)
		}
		return id
	}

	secret, _ := tokens.NewRandomSecret(32)
	signer, _ := tokens.New(secret)
	committer := clicks.NewCommitter(ctx, h.DB, 100, 20*time.Millisecond)
	t.Cleanup(committer.Close)
	mux := http.NewServeMux()
	mux.Handle("/r/", httpapi.NewRedirectHandler(h.DB, httpapi.Config{
		Signer:              signer,
		AllowedDestSuffixes: []string{"shop.io"},
		FallbackURL:         "https://shop.io/",
	}, committer))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	follow := func(code string) (tokenAff int64) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/r/"+code+"?dest="+url.QueryEscape("https://shop.io/pricing"), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Test")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		tok := loc.Query().Get("aff_token")
		if tok == "" {
			return 0
		}
		p, err := signer.Parse(tok)
		if err != nil {
			t.Fatalf("%s: token does not verify: %v", code, err)
		}
		return p.AffiliateID
	}

	jane, bob := idOf("jane@shop.io"), idOf("bob@shop.io")
	if got := follow("janeslink"); got != jane {
		t.Fatalf("/r/janeslink token for %d, want jane %d", got, jane)
	}
	if got := follow("JanesLink-2"); got != bob {
		t.Fatalf("/r/janeslink-2 token for %d, want bob %d", got, bob)
	}
	if got := follow("person"); got != 0 {
		t.Fatalf("frozen import tracked (token for %d)", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var nJane, nBob, total int
		h.DB.QueryRow(`SELECT COUNT(*) FROM clicks WHERE affiliate_id = ?`, jane).Scan(&nJane)
		h.DB.QueryRow(`SELECT COUNT(*) FROM clicks WHERE affiliate_id = ?`, bob).Scan(&nBob)
		h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&total)
		if nJane == 1 && nBob == 1 && total == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("clicks jane=%d bob=%d total=%d, want 1/1/2", nJane, nBob, total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// snapshot renders the rows an import may touch, order-independent.
func snapshot(t *testing.T, h *db.Handles) string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT 'aff', id, email, name, status FROM affiliates`,
		`SELECT 'code', affiliate_id, code, is_primary, source FROM affiliate_codes`,
		`SELECT 'coupon', affiliate_id, code, rate_type, active FROM coupons`,
	} {
		rows, err := h.DB.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, b, c, d, e any
			if err := rows.Scan(&a, &b, &c, &d, &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s|%v|%s|%v|%v", a, b, c, d, e))
		}
		rows.Close()
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

type undoFile struct {
	Source     string `json:"source"`
	Affiliates []struct {
		AffiliateID int64  `json:"affiliate_id"`
		Email       string `json:"email"`
		Code        string `json:"code"`
		Coupon      string `json:"coupon"`
		Created     bool   `json:"created"`
	} `json:"affiliates"`
}

// applyUndo reverses a batch the way an operator would from its undo file:
// drop the listed codes and coupons, then the affiliates it created.
func applyUndo(t *testing.T, h *db.Handles, batchID int64) undoFile {
	t.Helper()
	var sb strings.Builder
	if err := importer.RollbackExport(context.Background(), h.DB, batchID, &sb); err != nil {
		t.Fatal(err)
	}
	var u undoFile
	if err := json.Unmarshal([]byte(sb.String()), &u); err != nil {
		t.Fatal(err)
	}
	for _, a := range u.Affiliates {
		if a.Code != "" {
			h.DB.Exec(`DELETE FROM affiliate_codes WHERE affiliate_id = ? AND code = ?`, a.AffiliateID, a.Code)
		}
		if a.Coupon != "" {
			h.DB.Exec(`DELETE FROM coupons WHERE affiliate_id = ? AND code = ?`, a.AffiliateID, a.Coupon)
		}
	}
	for _, a := range u.Affiliates {
		if a.Created {
			if _, err := h.DB.Exec(`DELETE FROM affiliates WHERE id = ?`, a.AffiliateID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return u
}

// IM7: the undo file reverses exactly its own import. The database already
// holds one of the imported emails (with its own primary code and the
// very coupon the CSV names) and a code the CSV wants: applying the undo
// must restore that pre-state row for row, touching nothing that
// predates the import. (The payable ledger account a created affiliate
// gets is left behind: empty, and ledger rows are append-only.)
func TestMigrate_UndoReversesExactlyThisImport(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO affiliates (id, email, name) VALUES (50, 'jane@shop.io', 'Jane (native)'), (60, 'other@x.io', 'Other')`,
		`INSERT INTO affiliate_codes (affiliate_id, code, is_primary) VALUES (50, 'jane', TRUE), (60, 'janeslink', TRUE)`,
		`INSERT INTO coupons (code, affiliate_id) VALUES ('jane20', 50)`,
	} {
		if _, err := h.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, h)

	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))
	rep, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false)
	if err != nil {
		t.Fatal(err)
	}
	// jane keeps her native primary; her imported slug is an extra code.
	var primaries int
	h.DB.QueryRow(`SELECT COUNT(*) FROM affiliate_codes WHERE affiliate_id = 50 AND is_primary = TRUE`).Scan(&primaries)
	if primaries != 1 {
		t.Fatalf("jane has %d primary codes, want 1 (the native one)", primaries)
	}
	if snapshot(t, h) == before {
		t.Fatal("import changed nothing")
	}

	u := applyUndo(t, h, rep.BatchID)
	if after := snapshot(t, h); after != before {
		t.Fatalf("undo did not restore the pre-import state\nbefore:\n%s\nafter:\n%s\nundo: %+v", before, after, u)
	}
}

// IM8: re-running an import creates nothing (it used to mint
// janeslink-3, -4... for affiliates who already held their codes) and
// records no batch: the report points at the batch that did the work.
func TestMigrate_ReimportCreatesNothing(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))
	first, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Codes != 3 {
		t.Fatalf("first import codes=%d, want 3", first.Codes)
	}
	before := snapshot(t, h)
	second, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false)
	if err != nil {
		t.Fatal(err)
	}
	// bob's CSV code still collides with jane's (reported again), but it
	// resolves to the janeslink-2 he already holds.
	if second.Codes != 0 || second.CodeClashes != first.CodeClashes || snapshot(t, h) != before {
		t.Fatalf("re-import: codes=%d clashes=%d, changed=%v", second.Codes, second.CodeClashes, snapshot(t, h) != before)
	}
	if !second.NothingNew || second.BatchID != 0 || second.PreviousBatch != first.BatchID {
		t.Fatalf("re-import report: nothing_new=%v batch=%d previous=%d", second.NothingNew, second.BatchID, second.PreviousBatch)
	}
	var batches int
	h.DB.QueryRow(`SELECT COUNT(*) FROM import_batches`).Scan(&batches)
	if batches != 1 {
		t.Fatalf("re-import recorded a batch: %d batches", batches)
	}
}

// IM9: dry-run reports what the real run will do (clashes included) and
// writes nothing at all.
func TestMigrate_DryRunMatchesRealRun(t *testing.T) {
	h := newEnv(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (60, 'other@x.io')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO affiliate_codes (affiliate_id, code) VALUES (60, 'janeslink')`); err != nil {
		t.Fatal(err)
	}
	counts := func() string {
		var parts []string
		for _, tbl := range []string{"affiliates", "affiliate_codes", "coupons", "import_batches", "import_undos", "ledger_accounts"} {
			var n int
			h.DB.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n)
			parts = append(parts, fmt.Sprintf("%s=%d", tbl, n))
		}
		return strings.Join(parts, " ")
	}
	before := counts()
	affs, _, _ := importer.Parse(importer.Rewardful, strings.NewReader(fixtures["rewardful"].csv))
	dry, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, true)
	if err != nil {
		t.Fatal(err)
	}
	if after := counts(); after != before {
		t.Fatalf("dry-run wrote rows: %s -> %s", before, after)
	}
	real, err := importer.Migrate(ctx, h.DB, importer.Rewardful, affs, false)
	if err != nil {
		t.Fatal(err)
	}
	// jane: janeslink (taken) -> janeslink-2. bob: janeslink -> -2 (jane's
	// now) -> janeslink-3. Three clashes, three codes, in both modes.
	if dry.Codes != real.Codes || dry.CodeClashes != real.CodeClashes || dry.CodeClashes != 3 || dry.Codes != 3 {
		t.Fatalf("dry-run codes=%d clashes=%d, real codes=%d clashes=%d (want 3/3)",
			dry.Codes, dry.CodeClashes, real.Codes, real.CodeClashes)
	}
}
