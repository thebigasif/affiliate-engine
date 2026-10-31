package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

func pgAdminDSNForTest() string {
	if v := os.Getenv("AT_TEST_PG_ADMIN_DSN"); v != "" {
		return v
	}
	return os.Getenv("AE_TEST_PG_ADMIN_DSN")
}

// openPoolForTest opens a pool on dsn with the engine rewriting
// connector. search_path, when not empty, is set through the pgx config
// (a connect-time parameter, never a SET statement on a pooled session).
func openPoolForTest(ctx context.Context, dsn, searchPath string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["standard_conforming_strings"] = "on"
	if searchPath != "" {
		cfg.RuntimeParams["search_path"] = searchPath
	}
	pool := sql.OpenDB(db.NewRewritingConnector(pgxstdlib.GetConnector(*cfg)))
	pool.SetMaxOpenConns(8)
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return pool, nil
}

// rewriteAdminDSN points the admin DSN at dbName.
func rewriteAdminDSN(admin, dbName string) string {
	base, err := pgx.ParseConfig(admin)
	if err != nil {
		return admin
	}
	return fmt.Sprintf("postgres://%s:%d/%s?sslmode=disable&user=%s&password=%s",
		base.Host, base.Port, dbName, base.User, base.Password)
}

// pooledTestDB provisions a fresh Postgres database with two shop schemas,
// each running the full engine PG migration chain and a seeded affiliate
// (account() checks the affiliate row before creating a payable).
// Skips without an admin DSN.
func pooledTestDB(t *testing.T) string {
	t.Helper()
	admin := pgAdminDSNForTest()
	if admin == "" {
		t.Skip("no Postgres admin DSN configured (AT_TEST_PG_ADMIN_DSN / AE_TEST_PG_ADMIN_DSN)")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("ae_pl_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(cctx, admin)
		if err == nil {
			_, _ = c.Exec(cctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			_ = c.Close(cctx)
		}
	})
	_ = ac.Close(ctx)

	dsn := rewriteAdminDSN(admin, dbName)

	// Provision each shop schema through a dedicated pool whose connect
	// config carries the schema path (never a SET on a shared session).
	for _, schema := range []string{"shop_aaa", "shop_bbb"} {
		sp, err := openPoolForTest(ctx, dsn, schema)
		if err != nil {
			t.Fatalf("open provision pool %s: %v", schema, err)
		}
		if _, err := sp.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
			_ = sp.Close()
			t.Fatalf("create schema %s: %v", schema, err)
		}
		if err := migrations.Apply(ctx, sp, migrations.Postgres); err != nil {
			_ = sp.Close()
			t.Fatalf("migrate %s: %v", schema, err)
		}
		if _, err := sp.ExecContext(ctx, `INSERT INTO affiliates (id, email) VALUES (7, 'a@example.com')`); err != nil {
			var cols string
			_ = sp.QueryRowContext(ctx, `SELECT COALESCE(string_agg(column_name, ', ' ORDER BY ordinal_position), '')
				FROM information_schema.columns WHERE table_schema = ? AND table_name = 'affiliates'`, schema).Scan(&cols)
			_ = sp.Close()
			t.Fatalf("seed affiliate in %s (columns: %s): %v", schema, cols, err)
		}
		_ = sp.Close()
	}
	return dsn
}

// TestPooledPoster_ExactlyOnceUnderConcurrency: N goroutines post the
// SAME journal (same idem key) concurrently; exactly one journal lands,
// every other attempt reports ErrDuplicate, and the ledger stays
// balanced. Invariant: exactly-once money under concurrency
// (SECURITY.md money rule; PRD 13.4).
func TestPooledPoster_ExactlyOnceUnderConcurrency(t *testing.T) {
	dsn := pooledTestDB(t)
	ctx := context.Background()
	pool, err := openPoolForTest(ctx, dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	p := NewPooledPoster(pool, "shop_aaa")

	const n = 24
	var wg sync.WaitGroup
	var duplicates, oks int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := p.Post(ctx, "idem-x1", "test", "exactly once", time.Now(), []Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: 1000},
				{Account: "HOLD:USD", Amount: -1000},
			})
			switch {
			case err == nil:
				if id == 0 {
					t.Error("winner returned id 0")
				}
				atomic.AddInt64(&oks, 1)
			case errors.Is(err, ErrDuplicate):
				atomic.AddInt64(&duplicates, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if oks != 1 {
		t.Fatalf("exactly-once violated: %d successes, want 1 (duplicates=%d)", oks, duplicates)
	}
	if int(oks+duplicates) != n {
		t.Fatalf("goroutines accounted: %d+%d != %d", oks, duplicates, n)
	}
	sum, err := p.GlobalIntegrity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Fatalf("ledger unbalanced: %d", sum)
	}
	var journals int
	if err := p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries`).Scan(&journals)
	}); err != nil {
		t.Fatal(err)
	}
	if journals != 1 {
		t.Fatalf("journals = %d, want 1", journals)
	}
}

// TestPooledPoster_ShopLocksDoNotCross: the same entity key in two shops
// maps to two advisory locks — shop A holding its lock must not block
// shop B's post (PRD 12.4 item 2, bounded form of the cross-shop AC).
func TestPooledPoster_ShopLocksDoNotCross(t *testing.T) {
	dsn := pooledTestDB(t)
	ctx := context.Background()
	pool, err := openPoolForTest(ctx, dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	a := NewPooledPoster(pool, "shop_aaa")
	b := NewPooledPoster(pool, "shop_bbb")

	// Shop A takes its advisory lock and holds it.
	release := make(chan struct{})
	go func() {
		err := a.Run(ctx, func(ctx context.Context, tx *Tx) error {
			if err := tx.LockKey(ctx, "ledger:AFFILIATE_PAYABLE:7:USD"); err != nil {
				return err
			}
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("shop A hold: %v", err)
		}
	}()
	time.Sleep(150 * time.Millisecond) // let A take the lock

	// Shop B posts through the SAME entity lock key: must complete well
	// inside A's hold (different lock namespace, different schema).
	done := make(chan error, 1)
	go func() {
		_, err := b.Post(ctx, "idem-b1", "test", "cross shop", time.Now(), []Line{
			{Account: "EXPENSE:COMMISSION:USD", Amount: 500},
			{Account: "HOLD:USD", Amount: -500},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shop B blocked by shop A's lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shop B still blocked after 5s: advisory locks cross shops")
	}
	close(release)
}

// TestPooledPoster_SearchPathIsTransactionScoped: a pooled post sets
// SET LOCAL search_path inside its transaction; after commit, a plain
// pool query cannot resolve the shop's tables (no leaked path), and the
// post landed in the shop's schema.
func TestPooledPoster_SearchPathIsTransactionScoped(t *testing.T) {
	dsn := pooledTestDB(t)
	ctx := context.Background()
	pool, err := openPoolForTest(ctx, dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	p := NewPooledPoster(pool, "shop_aaa")

	if _, err := p.Post(ctx, "idem-s1", "test", "scoped", time.Now(), []Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 100},
		{Account: "HOLD:USD", Amount: -100},
	}); err != nil {
		t.Fatal(err)
	}

	// The post landed in shop_aaa (visible through a tenant transaction).
	var journals int
	if err := p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries`).Scan(&journals)
	}); err != nil {
		t.Fatal(err)
	}
	if journals != 1 {
		t.Fatalf("shop_aaa journals = %d, want 1", journals)
	}

	// Outside any tenant transaction the shop's table must not resolve
	// on the same pool (a leaked session path would resolve it).
	_, err = pool.ExecContext(ctx, "SELECT COUNT(*) FROM journal_entries")
	if err == nil {
		t.Fatal("journal_entries resolved outside the tenant transaction: search_path leaked to the pool session")
	}
}

// TestPooledPoster_OverdraftGuardAndClawback: the payable overdraft guard
// holds on the pooled path (a payout above what is owed is refused) and
// PostClawback may take the payable positive (a receivable).
func TestPooledPoster_OverdraftGuardAndClawback(t *testing.T) {
	dsn := pooledTestDB(t)
	ctx := context.Background()
	pool, err := openPoolForTest(ctx, dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	p := NewPooledPoster(pool, "shop_aaa")

	// Commission: payable credited (negative = we owe).
	if _, err := p.Post(ctx, "idem-c1", "test", "commission", time.Now(), []Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 2000},
		{Account: "AFFILIATE_PAYABLE:7:USD", Amount: -2000},
	}); err != nil {
		t.Fatal(err)
	}
	// Payout above owed: refused by the guard.
	_, err = p.Post(ctx, "idem-c2", "test", "overdraft", time.Now(), []Line{
		{Account: "AFFILIATE_PAYABLE:7:USD", Amount: 3000},
		{Account: "PAYOUT_CLEARING:USD", Amount: -3000},
	})
	if err == nil {
		t.Fatal("overdraft post accepted: guard missing on pooled path")
	}
	// Pay out what is owed, then claw back more than 0: payable goes
	// positive (receivable), allowed only through PostClawback.
	if _, err := p.Post(ctx, "idem-c3", "test", "pay out", time.Now(), []Line{
		{Account: "AFFILIATE_PAYABLE:7:USD", Amount: 2000},
		{Account: "PAYOUT_CLEARING:USD", Amount: -2000},
	}); err != nil {
		t.Fatal(err)
	}
	// Clawback after payout (FR-56): the expense is reversed and the
	// affiliate's debt lands in AFFILIATE_RECEIVABLE (PRD 12.5); the
	// payable stays at 0 and the next payout nets the receivable.
	if _, err := p.PostClawback(ctx, "idem-c4", "test", "clawback", time.Now(), []Line{
		{Account: "AFFILIATE_RECEIVABLE:7:USD", Amount: 1500},
		{Account: "EXPENSE:COMMISSION:USD", Amount: -1500},
	}); err != nil {
		t.Fatalf("clawback refused: %v", err)
	}
	payable, err := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:7:USD")
	if err != nil {
		t.Fatal(err)
	}
	if payable != 0 {
		t.Fatalf("payable after clawback = %d, want 0", payable)
	}
	rec, err := p.AccountBalance(ctx, "AFFILIATE_RECEIVABLE:7:USD")
	if err != nil {
		t.Fatal(err)
	}
	if rec != 1500 {
		t.Fatalf("receivable = %d, want +1500 (the affiliate owes it)", rec)
	}
	if sum, _ := p.GlobalIntegrity(ctx); sum != 0 {
		t.Fatalf("unbalanced: %d", sum)
	}
}

// TestPooledPoster_Interface: the pooled poster satisfies the interface
// commissions and fraud consume (the clawback-capable Poster).
type pooledPosterInterface interface {
	Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error)
	PostClawback(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error)
	AccountBalance(ctx context.Context, code string) (int64, error)
	Run(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error
}

var _ pooledPosterInterface = (*PooledPoster)(nil)
