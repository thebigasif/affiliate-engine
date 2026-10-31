// demo.go — the public read-only live demo (AE_DEMO=1).
//
// A demo install serves the real dashboard over a database built by
// demo-seed (demoseed.go). It keeps itself fresh: on start it (re)seeds
// when the database is empty or its seed is about a day old, and once the
// seed has aged a day it exits cleanly at the next 04:00 UTC, so the
// container's restart policy brings it back with a fresh seed. Everything
// read-only lives in httpapi (DemoGuard, the demo key).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/httpapi"
)

const (
	// demoMaxAge is how old a seed may get before it is replaced: a day,
	// less slack for the seed's own run time — a seed finished at 04:00:30
	// is still replaced at the next 04:00.
	demoMaxAge = 23 * time.Hour
	// demoRestartHourUTC is when an aged demo exits for a fresh seed
	// (quiet hours for EU and US prospects).
	demoRestartHourUTC = 4
)

// setupDemo readies demo mode when AE_DEMO=1 and reports whether it is on:
// seeds the database if needed and arms the daily restart (stop ends the
// serve loop: a clean exit, status 0). Runs before the server opens the
// database, because a reseed replaces the file.
func setupDemo(ctx context.Context, dsn string, stop func()) (bool, error) {
	if os.Getenv("AE_DEMO") != "1" {
		return false, nil
	}
	if isPostgresDSN(dsn) {
		return false, errors.New("AE_DEMO=1 needs a sqlite database (the demo replaces its file with a fresh seed daily)")
	}
	seededAt, err := ensureDemoSeed(ctx, sqlitePath(dsn), time.Now().UTC())
	if err != nil {
		return false, err
	}
	at := demoRestartAt(seededAt)
	fmt.Printf("affiliate-engine: live demo mode — read-only, public key %q; sample data seeded %s, fresh seed at %s\n",
		httpapi.DemoKey, seededAt.Format(time.RFC3339), at.Format(time.RFC3339))
	go func() {
		t := time.NewTimer(time.Until(at))
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
			fmt.Println("affiliate-engine: demo data is a day old; exiting for a fresh seed (the restart policy brings the demo back)")
			stop()
		}
	}()
	return true, nil
}

// demoGuard puts httpapi.DemoGuard in front of the whole server in demo
// mode (webhooks, MCP, /r/ and every write method are refused there).
func demoGuard(h http.Handler, on bool) http.Handler {
	if !on {
		return h
	}
	return httpapi.DemoGuard(h)
}

// ensureDemoSeed returns when the database at path was seeded, seeding it
// first when it is empty, half-seeded, or older than demoMaxAge. It never
// wipes a database demo-seed did not create: AE_DEMO=1 pointed at a real
// install fails instead of deleting the program.
func ensureDemoSeed(ctx context.Context, path string, now time.Time) (time.Time, error) {
	st, err := inspectDemoDB(ctx, path)
	if err != nil {
		return time.Time{}, err
	}
	reseed, err := demoNeedsSeed(st, now)
	if err != nil {
		return time.Time{}, fmt.Errorf("AE_DEMO=1: %s %w", path, err)
	}
	if !reseed {
		return st.seededAt, nil
	}
	if st.exists {
		if err := wipeSQLite(path); err != nil {
			return time.Time{}, err
		}
	}
	fmt.Println("affiliate-engine: seeding the demo program (well under a minute)...")
	res, err := seedDemo(ctx, path, now)
	if err != nil {
		return time.Time{}, err
	}
	return res.seededAt, nil
}

// demoNeedsSeed decides from what inspectDemoDB found: seed an empty
// database, reseed a half-finished or aged demo, keep a fresh one, and
// refuse anything demo-seed did not create.
func demoNeedsSeed(st demoDBState, now time.Time) (bool, error) {
	switch {
	case st.empty:
		return true, nil
	case !st.marked:
		return false, errors.New("holds data demo-seed did not create; the demo replaces its database daily, so give it its own volume")
	case st.complete && now.Sub(st.seededAt) < demoMaxAge:
		return false, nil
	}
	return true, nil
}

// demoRestartAt is when a demo seeded at seededAt exits for a fresh seed:
// the first 04:00 UTC once the seed is demoMaxAge old. After the first
// cycle that is every day at 04:00.
func demoRestartAt(seededAt time.Time) time.Time {
	due := seededAt.UTC().Add(demoMaxAge)
	at := time.Date(due.Year(), due.Month(), due.Day(), demoRestartHourUTC, 0, 0, 0, time.UTC)
	if at.Before(due) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// demoDBState is what inspectDemoDB found at a path.
type demoDBState struct {
	exists   bool // the file exists
	empty    bool // no program data at all
	marked   bool // demo-seed created it (demo_seed row present)
	complete bool // ...and finished
	seededAt time.Time
}

func inspectDemoDB(ctx context.Context, path string) (demoDBState, error) {
	var st demoDBState
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		st.empty = true
		return st, nil
	} else if err != nil {
		return st, err
	}
	st.exists = true
	h, err := openForCLI(ctx, path)
	if err != nil {
		return st, err
	}
	defer closeHandles(h)
	var seeded string
	err = h.DB.QueryRowContext(ctx, `SELECT seeded_at FROM demo_seed WHERE id = 1`).Scan(&seeded)
	switch {
	case err == nil:
		st.marked = true
		if t, perr := time.Parse(time.RFC3339, seeded); perr == nil {
			st.complete, st.seededAt = true, t
		}
	case errors.Is(err, sql.ErrNoRows), strings.Contains(err.Error(), "no such table"):
	default:
		return st, err
	}
	st.empty, err = dbIsEmpty(ctx, h.DB)
	return st, err
}

// dbIsEmpty reports whether a migrated database holds no program data.
func dbIsEmpty(ctx context.Context, dbh *sql.DB) (bool, error) {
	var found bool
	err := dbh.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM affiliates) OR EXISTS (SELECT 1 FROM campaigns)
		    OR EXISTS (SELECT 1 FROM commissions) OR EXISTS (SELECT 1 FROM clicks)
		    OR EXISTS (SELECT 1 FROM api_keys) OR EXISTS (SELECT 1 FROM affiliate_applications)`).Scan(&found)
	return !found, err
}

// wipeSQLite deletes a database file and its WAL siblings.
func wipeSQLite(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("demo: wipe %s: %w", p, err)
		}
	}
	return nil
}

// closeHandles releases a CLI-opened database (the ledger conn first: the
// pool cannot close while it is checked out).
func closeHandles(h *db.Handles) {
	_ = h.Ledger.Close()
	_ = h.DB.Close()
}
