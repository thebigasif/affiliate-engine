package commissions_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// pooledShop provisions a fresh Postgres database with one shop schema
// (the full engine chain, affiliate 7) and returns a pool scoped to it.
// Skips without an admin DSN.
func pooledShop(t *testing.T) (*sql.DB, string) {
	t.Helper()
	admin := os.Getenv("AT_TEST_PG_ADMIN_DSN")
	if admin == "" {
		admin = os.Getenv("AE_TEST_PG_ADMIN_DSN")
	}
	if admin == "" {
		t.Skip("no Postgres admin DSN configured")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("ae_cpp_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(cctx, admin); err == nil {
			_, _ = c.Exec(cctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			_ = c.Close(cctx)
		}
	})
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = dbName
	const schema = "shop_mo11"
	cfg.RuntimeParams["search_path"] = schema
	pool := sql.OpenDB(db.NewRewritingConnector(pgxstdlib.GetConnector(*cfg)))
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, pool, migrations.Postgres); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO affiliates (id, email) VALUES (7, 'a@example.com')`); err != nil {
		t.Fatal(err)
	}
	return pool, schema
}

func countJournals(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// MO-11: the engine's credit journal joins the PooledPoster transaction.
// A fail-closed fraud screen rolls the WHOLE credit back (zero journals,
// zero commissions); a redelivery then credits exactly once, and HOLD
// equals the pending commissions.
func TestPooledPoster_FailingScreenLeavesNoJournal(t *testing.T) {
	pool, schema := pooledShop(t)
	ctx := context.Background()
	poster := ledger.NewPooledPoster(pool, schema)
	eng := commissions.NewEngine(pool, poster)

	ev := commissions.Event{
		Source: "shopify", ExternalID: "gid://shopify/Order/1:default", EventType: "purchase",
		AmountMinor: 12000, FixedAmount: 1200, Currency: "USD", AffiliateID: 7,
		CampaignID: 1, OccurredAt: time.Now().UTC(),
	}
	screenErr := errors.New("rule error: fail closed")
	if _, err := eng.ProcessPurchaseScreened(ctx, ev, func(ctx context.Context, tx *ledger.Tx, id int64) error {
		return screenErr
	}); !errors.Is(err, screenErr) {
		t.Fatalf("screen error not surfaced: %v", err)
	}
	if n := countJournals(t, pool); n != 0 {
		t.Fatalf("failing screen left %d journal(s); want 0", n)
	}
	var nc int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM commissions`).Scan(&nc); err != nil {
		t.Fatal(err)
	}
	if nc != 0 {
		t.Fatalf("failing screen left %d commission(s)", nc)
	}

	// Redelivery with a passing screen: one credit, HOLD = -pending.
	id, err := eng.ProcessPurchaseScreened(ctx, ev, func(ctx context.Context, tx *ledger.Tx, id int64) error { return nil })
	if err != nil || id == 0 {
		t.Fatalf("redelivery: id=%d err=%v", id, err)
	}
	// A third delivery is a finished replay.
	if _, err := eng.ProcessPurchase(ctx, ev); !errors.Is(err, commissions.ErrAlreadyProcessed) {
		t.Fatalf("replay: %v", err)
	}
	if n := countJournals(t, pool); n != 1 {
		t.Fatalf("journals = %d; want 1", n)
	}
	hold, err := poster.AccountBalance(ctx, ledger.Hold("USD"))
	if err != nil {
		t.Fatal(err)
	}
	var pending int64
	if err := pool.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM commissions WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if hold != -pending || pending != 1200 {
		t.Fatalf("HOLD = %d, pending commissions = %d; want HOLD = -pending = -1200", hold, pending)
	}
	if sum, err := poster.GlobalIntegrity(ctx); err != nil || sum != 0 {
		t.Fatalf("integrity sum=%d err=%v", sum, err)
	}
}

// MO-11: a duplicate post returns the EXISTING journal id with
// ErrDuplicate (callers adopt it), never id 0.
func TestPooledPoster_DuplicateReturnsExistingID(t *testing.T) {
	pool, schema := pooledShop(t)
	ctx := context.Background()
	poster := ledger.NewPooledPoster(pool, schema)
	lines := []ledger.Line{{Account: ledger.Expense("USD"), Amount: 500}, {Account: ledger.Hold("USD"), Amount: -500}}
	id1, err := poster.Post(ctx, "dup:1", "test", "", time.Now(), lines)
	if err != nil || id1 == 0 {
		t.Fatalf("first post: id=%d err=%v", id1, err)
	}
	id2, err := poster.Post(ctx, "dup:1", "test", "", time.Now(), lines)
	if !errors.Is(err, ledger.ErrDuplicate) || id2 != id1 {
		t.Fatalf("duplicate post: id=%d err=%v; want id %d and ErrDuplicate", id2, err, id1)
	}
	// Joined inside a Run: the same contract, and an fn error rolls the
	// joined post back.
	boom := errors.New("boom")
	err = poster.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		id3, err := poster.Post(ctx, "dup:1", "test", "", time.Now(), lines)
		if !errors.Is(err, ledger.ErrDuplicate) || id3 != id1 {
			return fmt.Errorf("joined duplicate: id=%d err=%v", id3, err)
		}
		if _, err := poster.Post(ctx, "dup:2", "test", "", time.Now(), lines); err != nil {
			return err
		}
		bal, err := poster.AccountBalance(ctx, ledger.Hold("USD"))
		if err != nil {
			return err
		}
		if bal != -1000 { // sees its own uncommitted post
			return fmt.Errorf("joined balance = %d; want -1000", bal)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("run: %v", err)
	}
	if n := countJournals(t, pool); n != 1 {
		t.Fatalf("rolled-back joined post left journals: %d", n)
	}
}
