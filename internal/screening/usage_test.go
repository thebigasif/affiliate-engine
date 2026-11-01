package screening_test

// Usage (metered) commissions are screened in their settlement
// transaction, like purchases.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/screening"
	"github.com/thebigasif/affiliate-engine/internal/usage"
)

type usagePoster struct{ p *ledger.Poster }

func (a usagePoster) Post(ctx context.Context, idemKey, source, desc string, eff time.Time, lines []usage.Line) (int64, error) {
	ll := make([]ledger.Line, len(lines))
	for i, l := range lines {
		ll[i] = ledger.Line{Account: l.Account, Amount: l.Amount}
	}
	return a.p.Post(ctx, idemKey, source, desc, eff, ll)
}

func (a usagePoster) Run(ctx context.Context, fn func(ctx context.Context, tx usage.Tx) error) error {
	return a.p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error { return fn(ctx, tx) })
}

// settleUsage ingests one usage event for affiliate 1's customer and
// settles the day through a Settler screened by eng. It returns the usage
// commission's id.
func settleUsage(t *testing.T, h *db.Handles, eng *fraud.Engine) (int64, error) {
	t.Helper()
	ctx := context.Background()
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id)
		VALUES (1, 'cus_meter', 1)`); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC()
	if err := usage.New(h.DB).Ingest(ctx, usage.Event{
		Source: "api", ExternalID: "meter-1", CustomerExtID: "cus_meter",
		Units: 100000, UnitType: "tokens", OccurredAt: day,
	}); err != nil {
		t.Fatal(err)
	}
	set := usage.NewSettler(h.DB, usagePoster{poster})
	scr := screening.New(nil, eng, false)
	set.Screen = func(ctx context.Context, tx usage.Tx, id int64) error {
		return scr.ScreenUsage(ctx, tx.(*ledger.Tx), id)
	}
	var dayInt int64
	fmt.Sscanf(day.Format("20060102"), "%d", &dayInt)
	if _, err := set.SettleDay(ctx, dayInt, 300); err != nil {
		return 0, err
	}
	var id int64
	if err := h.DB.QueryRow(`SELECT c.id FROM commissions c JOIN journal_entries j ON j.id = c.journal_id WHERE j.source = 'usage_settlement'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, nil
}

func usageEnv(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO affiliates (id, email) VALUES (1, 'meter@t.dev')`); err != nil {
		t.Fatal(err)
	}
	return h
}

func seedSales(t *testing.T, h *db.Handles, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("sale-%d", i)
		if _, err := h.DB.Exec(`INSERT INTO journal_entries (idem_key, source, effective_at)
			VALUES (?, 'conversion', CURRENT_TIMESTAMP)`, key); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status, created_at)
			VALUES (1, (SELECT id FROM journal_entries WHERE idem_key = ?), 100, '{}', 'pending', ?)`,
			key, time.Now().UTC().Add(-5*time.Minute).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
}

func holdOf(t *testing.T, h *db.Handles, id int64) (string, bool) {
	t.Helper()
	var reason string
	err := h.DB.QueryRow(`SELECT reason FROM commission_holds WHERE commission_id = ?`, id).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return reason, true
}

func screenedAt(t *testing.T, h *db.Handles, id int64) bool {
	t.Helper()
	var screened bool
	if err := h.DB.QueryRow(`SELECT screened_at IS NOT NULL FROM commissions WHERE id = ?`, id).Scan(&screened); err != nil {
		t.Fatal(err)
	}
	return screened
}

// SU1: a clean usage commission is screened (screened_at set) and not held.
func TestScreening_UsageCommissionScreened(t *testing.T) {
	h := usageEnv(t)
	id, err := settleUsage(t, h, fraud.NewEngine(h.DB))
	if err != nil {
		t.Fatal(err)
	}
	if !screenedAt(t, h, id) {
		t.Fatal("usage commission not screened")
	}
	if r, held := holdOf(t, h, id); held {
		t.Fatalf("clean usage commission held: %s", r)
	}
}

// SU2: a usage commission of a paused partner is held by a rule that
// applies to usage (inactive_affiliate), in the settlement transaction.
// The rule runs on the commission: settlement itself skips paused partners,
// so the partner is paused inside the transaction, after the meter read.
func TestScreening_UsageCommissionHeldByRule(t *testing.T) {
	h := usageEnv(t)
	pausing := fraud.NewEngineWithRules(h.DB, pauseThen{fraud.NewEngine(h.DB).Rules()})
	id, err := settleUsage(t, h, pausing)
	if err != nil {
		t.Fatal(err)
	}
	r, held := holdOf(t, h, id)
	if !held || !strings.Contains(r, "inactive_affiliate") {
		t.Fatalf("usage commission hold = %q (held=%v), want inactive_affiliate", r, held)
	}
	if !screenedAt(t, h, id) {
		t.Fatal("usage commission not marked screened")
	}
}

// pauseThen pauses the affiliate (through the screening transaction), then
// runs rules.
type pauseThen struct{ rules []fraud.Rule }

func (pauseThen) Name() string { return "pause_then" }

func (p pauseThen) Evaluate(ctx context.Context, q fraud.Query) ([]fraud.Verdict, error) {
	if _, err := fraud.Conn(ctx, nil).ExecContext(ctx, `UPDATE affiliates SET status = 'frozen' WHERE id = ?`, q.AffiliateID); err != nil {
		return nil, err
	}
	var out []fraud.Verdict
	for _, r := range p.rules {
		vs, err := r.Evaluate(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

type usageBrokenRule struct{}

func (usageBrokenRule) Name() string { return "broken" }

func (usageBrokenRule) Evaluate(context.Context, fraud.Query) ([]fraud.Verdict, error) {
	return nil, errors.New("boom")
}

// SU3: screening fails closed on usage commissions too.
func TestScreening_UsageScreeningErrorHolds(t *testing.T) {
	h := usageEnv(t)
	id, err := settleUsage(t, h, fraud.NewEngineWithRules(h.DB, usageBrokenRule{}))
	if err != nil {
		t.Fatal(err)
	}
	if r, held := holdOf(t, h, id); !held || r != screening.ScreeningErrorReason {
		t.Fatalf("usage commission hold = %q (held=%v), want %s", r, held, screening.ScreeningErrorReason)
	}
}

// SU4: one settlement of many meters is not a burst of sales. 40 customers'
// usage settles into 40 commissions in one run: none is held, and the
// affiliate's earlier sale stays unheld.
func TestScreening_ManyMetersNotHeld(t *testing.T) {
	h := usageEnv(t)
	ctx := context.Background()
	seedSales(t, h, 1)
	poster := ledger.NewPoster(h.Ledger)
	if err := poster.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD": "liability", "EXPENSE:COMMISSION:USD": "expense",
	}); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC()
	ing := usage.New(h.DB)
	for i := 0; i < 40; i++ {
		cust := fmt.Sprintf("cus_m%02d", i)
		if _, err := h.DB.Exec(`INSERT INTO customers (program_id, external_customer_id, affiliate_id) VALUES (1, ?, 1)`, cust); err != nil {
			t.Fatal(err)
		}
		if err := ing.Ingest(ctx, usage.Event{Source: "api", ExternalID: "m-" + cust, CustomerExtID: cust,
			Units: 100000, UnitType: "tokens", OccurredAt: day}); err != nil {
			t.Fatal(err)
		}
	}
	set := usage.NewSettler(h.DB, usagePoster{poster})
	scr := screening.New(nil, fraud.NewEngine(h.DB), false)
	set.Screen = func(ctx context.Context, tx usage.Tx, id int64) error {
		return scr.ScreenUsage(ctx, tx.(*ledger.Tx), id)
	}
	var dayInt int64
	fmt.Sscanf(day.Format("20060102"), "%d", &dayInt)
	n, err := set.SettleDay(ctx, dayInt, 300)
	if err != nil {
		t.Fatal(err)
	}
	if n != 40 {
		t.Fatalf("settled %d, want 40", n)
	}
	var holds, unscreened int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM commission_holds`).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM commissions c JOIN journal_entries j ON j.id = c.journal_id
		WHERE j.source = 'usage_settlement' AND c.screened_at IS NULL`).Scan(&unscreened); err != nil {
		t.Fatal(err)
	}
	if holds != 0 || unscreened != 0 {
		t.Fatalf("holds=%d unscreened=%d, want 0 and 0", holds, unscreened)
	}
}
