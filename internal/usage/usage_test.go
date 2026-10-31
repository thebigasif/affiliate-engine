package usage_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/usage"
)

func newEnv(t *testing.T) (*db.Handles, *usage.Ingestor, *usage.Settler, *ledger.Poster) {
	t.Helper()
	ctx := context.Background()
	// Dual-engine (BUILD-PLAN §3): file-backed SQLite by default; PG when
	// AE_TEST_ENGINES selects it (CI runs both legs for this money path).
	h := dbtest.Open(t)
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
	}); err != nil {
		t.Fatal(err)
	}
	h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'ai@t.dev')`)
	h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id)
		VALUES (1, 'cus_ai', 1)`)
	ing := usage.New(h.DB)
	set := usage.NewSettler(h.DB, posterAdapter{poster})
	return h, ing, set, poster
}

// posterAdapter bridges usage.Poster to *ledger.Poster (identical shapes,
// distinct named types to keep the package dependency-light).
type posterAdapter struct{ p *ledger.Poster }

func (a posterAdapter) Post(ctx context.Context, idemKey, source, desc string, eff time.Time, lines []usage.Line) (int64, error) {
	ll := make([]ledger.Line, len(lines))
	for i, l := range lines {
		ll[i] = ledger.Line{Account: l.Account, Amount: l.Amount}
	}
	return a.p.Post(ctx, idemKey, source, desc, eff, ll)
}

func (a posterAdapter) Run(ctx context.Context, fn func(ctx context.Context, tx usage.Tx) error) error {
	return a.p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error { return fn(ctx, tx) })
}

func ev(id string, units int64) usage.Event {
	return usage.Event{
		Source: "api", ExternalID: id, CustomerExtID: "cus_ai",
		Units: units, UnitType: "tokens",
		OccurredAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
	}
}

// U1: ingest rolls the daily meter; duplicates are idempotent no-ops.
func TestUsage_Ingest(t *testing.T) {
	h, ing, _, _ := newEnv(t)
	ctx := context.Background()
	if err := ing.Ingest(ctx, ev("u1", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := ing.Ingest(ctx, ev("u2", 2500)); err != nil {
		t.Fatal(err)
	}
	if err := ing.Ingest(ctx, ev("u1", 1000)); err != usage.ErrDuplicate {
		t.Fatalf("want duplicate error, got %v", err)
	}
	var units int64
	h.DB.QueryRow(`SELECT units FROM usage_daily`).Scan(&units)
	if units != 3500 {
		t.Fatalf("meter=%d want 3500", units)
	}
	// Negative rejected.
	if err := ing.Ingest(ctx, ev("u3", -1)); err == nil {
		t.Fatal("negative units accepted")
	}
}

// U2: settlement converts meter to commission at bp rate; re-run no-ops.
func TestUsage_Settle(t *testing.T) {
	h, ing, set, poster := newEnv(t)
	ctx := context.Background()
	ing.Ingest(ctx, ev("s1", 100000)) // 100k tokens
	// Rate: 3000 bp = $0.30 per 1k tokens => 100k tokens -> 3000 cents.
	day := int64(20260915)
	n, err := set.SettleDay(ctx, day, 300)
	if err != nil || n != 1 {
		t.Fatalf("settle n=%d err=%v", n, err)
	}
	var amount int64
	var snapshot string
	h.DB.QueryRow(`SELECT amount, rate_snapshot FROM commissions`).Scan(&amount, &snapshot)
	if amount != 3000 {
		t.Fatalf("commission=%d want 3000 (100k tokens @ 0.30/1k)", amount)
	}
	if snapshot == "" || amount != 3000 {
		t.Fatalf("snapshot=%s", snapshot)
	}
	// Ledger balanced.
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
	// Re-settle: delta is zero, no double commission.
	n2, err := set.SettleDay(ctx, day, 300)
	if err != nil || n2 != 0 {
		t.Fatalf("re-settle n=%d err=%v (must be no-op)", n2, err)
	}
	var count int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&count)
	if count != 1 {
		t.Fatalf("commissions=%d after re-run", count)
	}
	// Rate change: only the delta settles (retroactive raises pay the diff).
	n3, err := set.SettleDay(ctx, day, 400) // 400 bp => basis 4000, delta 1000
	if err != nil || n3 != 1 {
		t.Fatalf("raise settle n=%d err=%v", n3, err)
	}
	var total int64
	h.DB.QueryRow(`SELECT SUM(amount) FROM commissions`).Scan(&total)
	if total != 4000 {
		t.Fatalf("total=%d want 4000 (delta adjustment)", total)
	}
}

// U3: unattributed customers settle nothing (no binding = no commission).
func TestUsage_UnattributedSkipped(t *testing.T) {
	h, ing, set, _ := newEnv(t)
	ctx := context.Background()
	lost := usage.Event{
		Source: "api", ExternalID: "orphan", CustomerExtID: "cus_nobody",
		Units: 999999, UnitType: "tokens",
		OccurredAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
	}
	if err := ing.Ingest(ctx, lost); err != nil {
		t.Fatal(err)
	}
	n, err := set.SettleDay(ctx, 20260915, 300)
	if err != nil || n != 0 {
		t.Fatalf("orphan commissioned: n=%d err=%v", n, err)
	}
	var count int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&count)
	if count != 0 {
		t.Fatal("orphan created commission rows")
	}
}

// F4 pin: pass-1 journal lands, pass-2 domain write fails, re-run MUST
// recover by adopting the existing journal — no duplicate-idem wedge, no
// stranded HOLD liability.
func TestUsage_SettleCrashRecovery(t *testing.T) {
	h, ing, set, poster := newEnv(t)
	ctx := context.Background()
	ing.Ingest(ctx, ev("f4run1", 100000)) // meter: cus_ai, tokens
	const day = int64(20260915)

	// Simulate: journal posted, domain tx never ran. Do pass 1 by hand.
	delta := int64(100000 * 300 / 10000) // 3000 cents at 300bp... rate=300 -> 3000
	_, err := poster.Post(ctx,
		fmt.Sprintf("usage:%d:%s:%s:from:%d", day, "cus_ai", "tokens", 0),
		"usage_settlement", "cus_ai", time.Unix(0, 0).UTC(),
		[]ledger.Line{
			{Account: "EXPENSE:COMMISSION:USD", Amount: delta},
			{Account: "HOLD:USD", Amount: -delta},
		})
	if err != nil {
		t.Fatal(err)
	}
	// Domain rows absent (the "crash"): no commission, settled marker stale.
	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	if n != 0 {
		t.Fatalf("precondition: commissions=%d", n)
	}

	// Recovery run: adopts the journal, writes domain rows.
	got, err := set.SettleDay(ctx, day, 300)
	if err != nil {
		t.Fatalf("F4 wedge: recovery failed: %v", err)
	}
	if got != 1 {
		t.Fatalf("recovery commissions=%d want 1", got)
	}
	h.DB.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("domain rows after recovery=%d", n)
	}
	var settled int64
	h.DB.QueryRow(`SELECT settled_cents FROM usage_daily WHERE customer_ext_id='cus_ai'`).Scan(&settled)
	if settled != delta {
		t.Fatalf("settled=%d want %d", settled, delta)
	}
	if exp, _ := poster.AccountBalance(ctx, "EXPENSE:COMMISSION:USD"); exp != delta {
		t.Fatalf("expensed=%d want %d (journal adopted, not posted twice)", exp, delta)
	}
	// Idempotent: re-run is a no-op.
	again, err := set.SettleDay(ctx, day, 300)
	if err != nil || again != 0 {
		t.Fatalf("re-run n=%d err=%v", again, err)
	}
}

// U6 (regression, 2026-09-25): usage-settled commissions MUST carry a
// matures_at deadline. SettleDay once inserted NULL, and Mature() selects
// `matures_at <= now` — NULL never compares true on either engine, so
// every usage commission was stuck 'pending' forever with its money
// stranded in HOLD (FR-16 broken end-to-end). Prove the full loop:
// settle -> matures_at is a real deadline -> Mature() approves it and
// moves HOLD -> payable.
func TestUsage_SettleMatures(t *testing.T) {
	h, ing, set, poster := newEnv(t)
	ctx := context.Background()
	ing.Ingest(ctx, ev("m1", 100000))
	const day = int64(20260915)
	if n, err := set.SettleDay(ctx, day, 300); err != nil || n != 1 {
		t.Fatalf("settle n=%d err=%v", n, err)
	}
	var matures sql.NullString
	if err := h.DB.QueryRow(`SELECT matures_at FROM commissions`).Scan(&matures); err != nil {
		t.Fatal(err)
	}
	if !matures.Valid || matures.String == "" {
		t.Fatalf("matures_at is NULL/empty — Mature() would never approve it (the bug)")
	}
	mt, err := time.Parse(time.RFC3339Nano, matures.String)
	if err != nil {
		// PG returns time.Time through sql.NullString on timestamptz scans.
		t.Fatalf("matures_at=%q: %v", matures.String, err)
	}
	if d := time.Until(mt); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("matures_at is %v out; want ~30d (NET terms default)", d)
	}

	// Age the deadline past and run the maturation worker: the commission
	// must approve and the money must move HOLD -> payable (the loop that
	// never closed before).
	if _, err := h.DB.Exec(`UPDATE commissions SET matures_at = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	eng := commissions.NewEngine(h.DB, poster)
	n, err := eng.Mature(ctx)
	if err != nil || n != 1 {
		t.Fatalf("mature n=%d err=%v (usage commission must be approvable)", n, err)
	}
	var status string
	h.DB.QueryRow(`SELECT status FROM commissions`).Scan(&status)
	if status != "approved" {
		t.Fatalf("status=%q want approved", status)
	}
	bal, err := poster.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil {
		t.Fatal(err)
	}
	if bal != -3000 {
		t.Fatalf("payable=%d want -3000 (liability convention)", bal)
	}
	if s, _ := poster.GlobalIntegrity(ctx); s != 0 {
		t.Fatalf("integrity=%d", s)
	}
}

// U7 (heal): installs that settled usage commissions before this fix have
// matures_at = NULL rows stuck pending forever. The usage_* migration now
// heals them (NET terms from created_at). Re-applying migrations is the
// documented idempotent path an upgrade takes. Verified on both engines.
func TestUsage_MigrationHealsNullMatures(t *testing.T) {
	h, _, _, _ := newEnv(t)
	ctx := context.Background()
	// A stuck row: usage-origin, pending, NULL matures_at.
	h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at)
		VALUES ('heal1', 'usage_settlement', ?)`, time.Now().UTC().Format(time.RFC3339))
	res, err := h.DB.Exec(`INSERT INTO commissions
		(affiliate_id, campaign_id, journal_id, amount, rate_snapshot, status, matures_at, created_at)
		VALUES (1, 1, (SELECT id FROM journal_entries WHERE idem_key='heal1'), 500,
		?, 'pending', NULL, ?)`,
		`{"origin":"usage","unit_type":"tokens","units":100,"rate_bp":300}`,
		time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	// A purchase-origin NULL row must NOT be touched by the usage heal.
	h.DB.Exec(`INSERT INTO commissions
		(affiliate_id, campaign_id, journal_id, amount, rate_snapshot, status, matures_at, created_at)
		VALUES (1, 1, (SELECT id FROM journal_entries WHERE idem_key='heal1'), 900,
		'{"rule":"purchase"}', 'pending', NULL, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	_ = res

	// The upgrade path: re-apply migrations (idempotent).
	if err := migrations.Apply(ctx, h.DB, dbtest.Engine()); err != nil {
		t.Fatalf("re-apply migrations: %v", err)
	}
	var healed sql.NullString
	if err := h.DB.QueryRow(`SELECT matures_at FROM commissions
		WHERE amount = 500`).Scan(&healed); err != nil {
		t.Fatal(err)
	}
	if !healed.Valid || healed.String == "" {
		t.Fatalf("usage row not healed: matures_at=%v", healed)
	}
	// Healed deadline = created_at + 30d, so ~27d out (created 72h ago).
	mt, err := time.Parse(time.RFC3339Nano, healed.String)
	if err == nil {
		if d := time.Until(mt); d < 26*24*time.Hour || d > 28*24*time.Hour {
			t.Fatalf("healed deadline %v out; want ~27d (created_at+30d)", d)
		}
	}
	// The purchase row stays NULL (different fix domain; none invented here).
	var purchaseRow sql.NullString
	if err := h.DB.QueryRow(`SELECT matures_at FROM commissions
		WHERE amount = 900`).Scan(&purchaseRow); err != nil {
		t.Fatal(err)
	}
	if purchaseRow.Valid {
		t.Fatalf("purchase-origin row was unexpectedly modified: %v", purchaseRow.String)
	}
}
