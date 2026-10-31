// Package db opens the database for either engine and enforces the
// measured PRAGMA block on SQLite. It owns the long-lived handles:
// callers must NOT close them (owner rule).
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	// PostgreSQL driver (pure Go, CGO-free). Registered under a private
	// name; opened through the rewriting connector so `?` placeholders in
	// shared DML work unchanged on both engines.
	"github.com/jackc/pgx/v5"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
)

// pgDriverName is the wrapped pgx driver registration.

// Options configures the opener.
type Options struct {
	// LedgerSyncFull puts synchronous=FULL on the ledger (money) connection.
	LedgerSyncFull bool
}

// Handles bundles the connection topology: one dedicated write connection
// for money operations, a general pool, and a read pool.
type Handles struct {
	DB     *sql.DB   // general pool (analytics, reads, light writes)
	Ledger *sql.Conn // dedicated money-write connection (synchronous=FULL on SQLite)
}

// Open parses dsn ("sqlite:<path>" or "postgres:<dsn>") and returns handles.
// The caller never closes; handles live for the process lifetime.
func Open(ctx context.Context, dsn string, opts Options) (*Handles, error) {
	switch {
	case strings.HasPrefix(dsn, "sqlite:"):
		path := strings.TrimPrefix(dsn, "sqlite:")
		return openSQLite(ctx, path, opts)
	case strings.HasPrefix(dsn, "postgres:"), strings.HasPrefix(dsn, "postgresql:"):
		return openPostgres(ctx, dsn, opts)
	default:
		// The DSN itself is deliberately not echoed: it may carry a password.
		return nil, fmt.Errorf("db: unrecognized dsn scheme (want sqlite:<path> or postgres:<dsn>)")
	}
}

// openPostgres opens the pool through the rewriting connector (`?` -> `$N`)
// and takes a dedicated "ledger" connection from the same pool. PG handles
// its own durability and concurrency; there is no SQLite-style PRAGMA work
// and no single-writer constraint, so the ledger conn is simply a pinned
// session (serializing money posts) rather than a sync-mode knob.
//
// simple_protocol: multi-statement scripts (migrations) and extended-
// protocol-hostile payloads go through the simple protocol; parameterized
// DML still uses the extended protocol with rewritten placeholders.
func openPostgres(ctx context.Context, dsn string, opts Options) (*Handles, error) {
	_ = opts // no PRAGMA work on PG
	raw := strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres:"), "postgresql:")
	uri := "postgres://" + strings.TrimPrefix(raw, "//")

	cfg, err := pgx.ParseConfig(uri)
	if err != nil {
		return nil, fmt.Errorf("db: parse pg dsn: %w", err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["standard_conforming_strings"] = "on"
	// Timestamps read back as text (and date math) are UTC, like SQLite's,
	// whatever the server's default TimeZone. A DSN that sets one wins.
	if _, ok := cfg.RuntimeParams["timezone"]; !ok {
		cfg.RuntimeParams["timezone"] = "UTC"
	}
	pool := sql.OpenDB(utf8ConnReset{rewritingConnector{pgxstdlib.GetConnector(*cfg)}})
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(8)
	pool.SetConnMaxLifetime(0) // never recycle (never-close rule)

	ledger, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("db: ledger conn: %w", err)
	}
	if err := ping(ctx, ledger, 10*time.Second); err != nil {
		_ = ledger.Close()
		_ = pool.Close()
		return nil, err
	}
	return &Handles{DB: pool, Ledger: ledger}, nil
}

// utf8ConnReset ensures database/sql validates sessions on reuse.
type utf8ConnReset struct{ inner driver.Connector }

func (u utf8ConnReset) Connect(ctx context.Context) (driver.Conn, error) {
	return u.inner.Connect(ctx)
}
func (u utf8ConnReset) Driver() driver.Driver { return u.inner.Driver() }

// openSQLite opens the general pool and takes a dedicated "ledger" connection
// from it. Money writes run ONLY on that conn (Poster), under synchronous=FULL.
//
// BUG-CLASS NOTE (session 17): with the driver default DEFERRED tx, Post's
// idem-key SELECT takes a read snapshot, and if any OTHER connection commits
// a write between that SELECT and Post's INSERT, the deferred->write upgrade
// fails with SQLITE_BUSY_SNAPSHOT — the class busy_timeout CANNOT wait out
// (the snapshot is stale; waiting cannot fix it). Observed once in CI-shaped
// load (session 16 transient) and reproduced locally under full-suite load:
// Mature's post failed fast, its fire-and-forget revert also failed, and
// Uphold then clawed from the account of a status that lied — HOLD stranded.
//
// Fix: _txlock=immediate in the shared DSN — every BEGIN takes the write
// lock up front under busy_timeout, so the snapshot-upgrade class cannot
// arise anywhere (ledger posts AND the domain txs that race them). This is
// the risk-register's "BEGIN IMMEDIATE vs FOR UPDATE" discipline applied
// globally; all explicit txs in this codebase are writer-shaped, and WAL
// readers outside txs are unaffected.
func openSQLite(ctx context.Context, path string, opts Options) (*Handles, error) {
	// Measured configuration (research/data-layer/sqlite-ops.md):
	// WAL is 3x throughput; NORMAL everywhere except the ledger conn
	// (FULL there); busy_timeout to survive writer contention.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"+
			"&_pragma=busy_timeout(5000)&_pragma=cache_size(-64000)"+
			"&_pragma=mmap_size(268435456)&_pragma=foreign_keys(1)"+
			"&_txlock=immediate",
		path,
	)
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open sqlite: %w", err)
	}
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(8)
	pool.SetConnMaxLifetime(0) // never recycle (never-close rule)

	// Ledger connection: dedicated, synchronous=FULL (SQLite: PRAGMA below).
	ledger, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("db: ledger conn: %w", err)
	}
	if opts.LedgerSyncFull {
		if _, err := ledger.ExecContext(ctx, "PRAGMA synchronous=FULL;"); err != nil {
			_ = ledger.Close()
			_ = pool.Close()
			return nil, fmt.Errorf("db: set ledger synchronous=FULL: %w", err)
		}
	}
	if err := ping(ctx, ledger, 5*time.Second); err != nil {
		_ = ledger.Close()
		_ = pool.Close()
		return nil, err
	}
	return &Handles{DB: pool, Ledger: ledger}, nil
}

// IsPostgres reports whether c is a Postgres session (opened through the
// rewriting connector). Money code uses it to pick its locking: row and
// advisory locks exist only on Postgres; on SQLite every write transaction
// already holds the database write lock from BEGIN (_txlock=immediate).
func IsPostgres(c *sql.Conn) bool {
	pg := false
	_ = c.Raw(func(dc any) error {
		_, pg = dc.(rewritingConn)
		return nil
	})
	return pg
}

func ping(ctx context.Context, c *sql.Conn, timeout time.Duration) error {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := c.ExecContext(ctx2, "SELECT 1;"); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}
