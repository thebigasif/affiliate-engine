// Package dbtest opens a per-test database on the engine(s) selected by
// AE_TEST_ENGINES (default "sqlite"; comma-separated list). PG is used
// only when an admin DSN is configured via AE_TEST_PG_ADMIN_DSN —
// otherwise the postgres entry is skipped with a clear message.
//
// Dual-engine rule (BUILD-PLAN): money/invariant suites run on SQLite
// (file-backed, never :memory:) AND on Postgres when a runtime is
// available. CI supplies both; local runs degrade to SQLite alone.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// Engine returns the engine this run should use. With multiple engines
// configured, run the suite once per engine via `go test` (CI does this);
// within one process the first entry wins.
func Engine() migrations.Engine {
	engines := Engines()
	if len(engines) == 0 {
		return migrations.SQLite
	}
	return engines[0]
}

// IsPostgres reports whether the engine this run actually uses is
// Postgres. Tests that need dialect-specific DDL (trigger syntax, error
// introspection) MUST branch on this — never on raw AE_TEST_ENGINES
// string matching, which misparses the documented comma-separated list
// ("postgres,sqlite" fails an == check; "sqlite,postgres" fools a
// Contains check while Engine() runs SQLite).
func IsPostgres() bool { return Engine() == migrations.Postgres }

// Engines parses AE_TEST_ENGINES (default ["sqlite"]).
func Engines() []migrations.Engine {
	raw := os.Getenv("AE_TEST_ENGINES")
	if strings.TrimSpace(raw) == "" {
		raw = "sqlite"
	}
	var out []migrations.Engine
	for _, e := range strings.Split(raw, ",") {
		switch strings.TrimSpace(strings.ToLower(e)) {
		case "sqlite":
			out = append(out, migrations.SQLite)
		case "postgres", "pg":
			out = append(out, migrations.Postgres)
		}
	}
	return out
}

// Open returns migrated handles on the selected engine. SQLite is
// file-backed under t.TempDir() (in-memory is banned for concurrency
// semantics). PG gets a fresh database per test, dropped on cleanup.
func Open(t *testing.T) *db.Handles {
	t.Helper()
	h, _ := OpenDSN(t)
	return h
}

// OpenDSN is Open plus the db.Open DSN of the database, so a test can
// open it again — from another connection set or another process.
func OpenDSN(t *testing.T) (*db.Handles, string) {
	t.Helper()
	ctx := context.Background()
	switch Engine() {
	case migrations.Postgres:
		return openPostgres(t, ctx)
	default:
		return openSQLite(t, ctx)
	}
}

func openSQLite(t *testing.T, ctx context.Context) (*db.Handles, string) {
	t.Helper()
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "test.db")
	h, err := db.Open(ctx, dsn, db.Options{LedgerSyncFull: true})
	if err != nil {
		t.Fatalf("dbtest: open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		t.Fatalf("dbtest: migrate sqlite: %v", err)
	}
	return h, dsn
}

func openPostgres(t *testing.T, ctx context.Context) (*db.Handles, string) {
	t.Helper()
	admin := os.Getenv("AE_TEST_PG_ADMIN_DSN")
	if admin == "" {
		t.Skipf("AE_TEST_ENGINES=postgres but AE_TEST_PG_ADMIN_DSN not set; skipping PG leg")
	}
	base, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatalf("dbtest: parse admin dsn: %v", err)
	}
	dbName := fmt.Sprintf("ae_t_%s", strings.ToLower(randName(t)))

	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("dbtest: admin connect: %v", err)
	}
	if _, err := adminConn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("dbtest: create database %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(dropCtx, admin)
		if err != nil {
			return
		}
		defer c.Close(dropCtx)
		_, _ = c.Exec(dropCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
	})

	// Build the per-test DSN from the admin config (same auth, new db).
	// db.Open expects the full postgres:// URI (it strips and re-adds the
	// scheme internally), so pass url.String() unsuffixed.
	u := &url.URL{
		Scheme:   "postgres",
		Host:     fmt.Sprintf("%s:%d", base.Host, base.Port),
		User:     url.UserPassword(base.User, base.Password),
		Path:     "/" + dbName,
		RawQuery: "sslmode=disable",
	}
	h, err := db.Open(ctx, u.String(), db.Options{})
	if err != nil {
		t.Fatalf("dbtest: open pg: %v", err)
	}
	t.Cleanup(func() { _ = h.Ledger.Close(); _ = h.DB.Close() })
	if err := migrations.Apply(ctx, h.DB, migrations.Postgres); err != nil {
		t.Fatalf("dbtest: migrate pg: %v", err)
	}
	return h, u.String()
}

// randName derives a short unique suffix from the test name plus entropy
// (survives crashed runs that left databases behind).
func randName(t *testing.T) string {
	t.Helper()
	src := t.Name()
	var h uint32 = 2166136261
	for i := 0; i < len(src); i++ {
		h ^= uint32(src[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x_%d", h, time.Now().UnixNano()%1e9)
}

// At is a UTC timestamp d from now as RFC 3339 text, for test SQL that
// needs a relative time on every engine (SQLite's datetime('now', ...)
// does not exist on Postgres).
func At(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }
