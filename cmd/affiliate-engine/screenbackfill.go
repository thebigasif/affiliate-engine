package main

// screenbackfill.go — the `screen-backfill` subcommand: fraud screening for
// the commissions that releases before commissions.screened_at credited
// and never screened.
//
//	affiliate-engine -db sqlite:ae.db screen-backfill -dry-run
//	affiliate-engine -db sqlite:ae.db screen-backfill [-limit N] [-since 2026-01-01]
//
// Each commission is screened in its own ledger transaction with the same
// rules, verdicts and hold as a fresh credit (screening.Engine.Backfill).
// Safe while the server runs: SQLite writes wait their turn on the single
// writer, Postgres takes the commission's row lock. A second run finds
// nothing; an interrupted one picks up where it stopped.
//
// It is an operator command, not a startup step: every commission credited
// before screened_at existed has it NULL, including the ones the old
// release did screen, and the rules read data that arrived after the sale.
// On the demo data (all screened at credit) a pass holds 48 of 660 again.
// The operator runs -dry-run, reads the list, and decides.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

// backfillPace spaces the transactions, so a server writing to the same
// SQLite file gets the writer between them.
const backfillPace = 10 * time.Millisecond

func runScreenBackfill(dsn string, args []string) error {
	fs := flag.NewFlagSet("screen-backfill", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be held; write nothing")
	limit := fs.Int("limit", 0, "screen at most N commissions (0 = all)")
	sinceFlag := fs.String("since", "", "only commissions created on or after this date (YYYY-MM-DD)")
	jsonOut := fs.Bool("json", false, "machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit < 0 {
		return fmt.Errorf("screen-backfill: -limit must be >= 0")
	}
	var since time.Time
	if *sinceFlag != "" {
		t, err := time.Parse(time.DateOnly, *sinceFlag)
		if err != nil {
			return fmt.Errorf("screen-backfill: -since wants YYYY-MM-DD: %w", err)
		}
		since = t
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := openDSN(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = h.Ledger.Close(); _ = h.DB.Close() }()

	shadow := os.Getenv("AE_FRAUD_DRYRUN") == "1"
	rep, err := newBackfillScreener(h, shadow).Backfill(ctx, h.DB, screening.BackfillOptions{
		DryRun: *dryRun, Limit: *limit, Since: since, Pace: backfillPace,
	})
	if *jsonOut {
		js, jerr := json.MarshalIndent(rep, "", "  ")
		if jerr != nil {
			return jerr
		}
		fmt.Println(string(js))
	} else {
		fmt.Println("screen-backfill: " + backfillSummary(rep, shadow))
		for _, hd := range rep.Holds {
			fmt.Printf("  commission %d (affiliate %d): %s\n", hd.CommissionID, hd.AffiliateID, hd.Reason)
		}
	}
	if err != nil {
		return fmt.Errorf("screen-backfill: stopped after %d: %w (run it again to continue)", rep.Scanned, err)
	}
	if rep.Errors > 0 {
		return fmt.Errorf("screen-backfill: %d commissions could not be screened (see the log above); run it again to retry them", rep.Errors)
	}
	return nil
}

// newBackfillScreener wires the screening decorator as the server does:
// the commissions engine on the ledger's dedicated connection (Poster.Run)
// and the fraud engine on the pool.
func newBackfillScreener(h *db.Handles, shadow bool) *screening.Engine {
	engine := commissions.NewEngine(h.DB, ledger.NewPoster(h.Ledger))
	return screening.New(engine, fraud.NewEngine(h.DB), shadow)
}

// backfillSummary is the one-line human report.
func backfillSummary(rep screening.BackfillReport, shadow bool) string {
	held := "held"
	if rep.DryRun {
		held = "would hold"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "scanned %d, screened %d, %s %d", rep.Scanned, rep.Screened, held, rep.Held)
	if len(rep.HeldBy) > 0 {
		var by []string
		for _, r := range slices.Sorted(maps.Keys(rep.HeldBy)) {
			by = append(by, fmt.Sprintf("%s %d", r, rep.HeldBy[r]))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(by, ", "))
	}
	if rep.AlsoHeld > 0 {
		fmt.Fprintf(&b, ", %s %d more through their clicks", held, rep.AlsoHeld)
	}
	fmt.Fprintf(&b, ", skipped %d, errors %d", rep.Skipped, rep.Errors)
	if rep.DryRun {
		b.WriteString(" [dry run: nothing written]")
	}
	if shadow {
		b.WriteString(" [AE_FRAUD_DRYRUN=1: verdicts recorded, nothing held]")
	}
	return b.String()
}
