package main

// import.go — the `import` and `compare` subcommands: migrate an incumbent
// affiliate program (Rewardful / FirstPromoter / Tapfiliate / Tolt CSV
// exports) into this engine, review it first, undo it in one command, and
// compare the two trackers side by side while both run.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/importer"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

const importUsage = "usage: affiliate-engine -db <dsn> import -from <source> -csv <affiliates.csv> [-referrals <file>] [-commissions <file>] [-dry-run] [-json]\n" +
	"       affiliate-engine -db <dsn> import -undo <batch id> [-json]\n" +
	"       affiliate-engine -db <dsn> import -list\n" +
	"       affiliate-engine -db <dsn> import -rollback-batch <batch id>"

// openImportDB opens and migrates the target database.
func openImportDB(ctx context.Context, dsn string) (*sql.DB, func(), error) {
	if dsn == "" {
		return nil, nil, fmt.Errorf("requires -db")
	}
	h, err := db.Open(ctx, dsn, db.Options{LedgerSyncFull: true})
	if err != nil {
		return nil, nil, err
	}
	closer := func() { _ = h.Ledger.Close(); _ = h.DB.Close() }
	eng := migrations.SQLite
	if isPostgresDSN(dsn) {
		eng = migrations.Postgres
	}
	if err := migrations.Apply(ctx, h.DB, eng); err != nil {
		closer()
		return nil, nil, err
	}
	return h.DB, closer, nil
}

func runImport(dsn string, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	srcFlag := fs.String("from", "", "incumbent source: rewardful|firstpromoter|tapfiliate|tolt")
	fileFlag := fs.String("csv", "", "path to the incumbent's affiliates CSV export")
	refFlag := fs.String("referrals", "", "optional: the incumbent's referrals CSV export (customer ids become bindings)")
	comFlag := fs.String("commissions", "", "optional: the incumbent's commissions CSV export (reviewed, never imported)")
	dryRun := fs.Bool("dry-run", false, "review only: counts, conflicts and fraud, nothing written")
	undo := fs.Int64("undo", 0, "reverse one import batch (refuses if money moved since)")
	list := fs.Bool("list", false, "list import batches")
	rollback := fs.Int64("rollback-batch", 0, "print a completed batch's undo file (JSON: what that import created) to stdout")
	jsonOut := fs.Bool("json", false, "machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	if *undo == 0 && !*list && *rollback == 0 && (*srcFlag == "" || *fileFlag == "") {
		return errors.New(importUsage)
	}
	var src importer.Source
	if *srcFlag != "" {
		var ok bool
		if src, ok = importer.ParseSource(*srcFlag); !ok {
			return fmt.Errorf("unknown source %q (want rewardful|firstpromoter|tapfiliate|tolt)", *srcFlag)
		}
	}
	h, closeDB, err := openImportDB(ctx, dsn)
	if err != nil {
		return fmt.Errorf("import %w", err)
	}
	defer closeDB()

	switch {
	case *rollback > 0:
		return importer.RollbackExport(ctx, h, *rollback, os.Stdout)
	case *list:
		bs, err := importer.Batches(ctx, h)
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(os.Stdout).Encode(bs)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "BATCH\tSOURCE\tAFFILIATES\tIMPORTED\tUNDONE")
		for _, b := range bs {
			fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\n", b.ID, b.Source, b.Affiliates, b.ImportedAt, or(b.UndoneAt, "-"))
		}
		return tw.Flush()
	case *undo > 0:
		rep, err := importer.Undo(ctx, h, *undo)
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(os.Stdout).Encode(rep)
		}
		if rep.AlreadyUndone {
			fmt.Printf("batch %d was already undone: nothing to do\n", rep.BatchID)
			return nil
		}
		fmt.Printf("undone batch %d (%s): removed affiliates=%d codes=%d coupons=%d customer_bindings=%d payable_accounts=%d\n",
			rep.BatchID, rep.Source, rep.Affiliates, rep.Codes, rep.Coupons, rep.Bindings, rep.LedgerAccounts)
		return nil
	}

	ds, preport, err := readExport(src, *fileFlag, *refFlag, *comFlag)
	if err != nil {
		return err
	}
	if !*jsonOut {
		fmt.Printf("parsed %d rows: %d affiliates, %d skipped, %d dup emails\n",
			preport.Rows, preport.Affiliates, preport.Skipped, preport.DupEmails)
	}
	report, err := importer.Run(ctx, h, src, ds, preport, *dryRun)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	printImportReport(os.Stdout, src, report)
	for _, w := range report.Warnings {
		fmt.Fprintf(os.Stderr, "  warning: %s\n", w)
	}
	if !*dryRun && report.BatchID > 0 {
		fmt.Printf("\nUndo this import with: affiliate-engine -db <dsn> import -undo %d\n", report.BatchID)
		printImportNextSteps(os.Stdout, src, os.Getenv("AE_PUBLIC_URL"))
	}
	return nil
}

// readExport opens the affiliates file and the optional history files and
// hands them to importer.ReadDataset.
func readExport(src importer.Source, affPath, refPath, comPath string) (importer.Dataset, *importer.Report, error) {
	f, err := os.Open(affPath) // #nosec G304 -- operator-supplied CLI path
	if err != nil {
		return importer.Dataset{}, nil, err
	}
	defer f.Close()
	var refs, coms io.Reader
	if refPath != "" {
		rf, err := os.Open(refPath) // #nosec G304 -- operator-supplied CLI path
		if err != nil {
			return importer.Dataset{}, nil, err
		}
		defer rf.Close()
		refs = rf
	}
	if comPath != "" {
		cf, err := os.Open(comPath) // #nosec G304 -- operator-supplied CLI path
		if err != nil {
			return importer.Dataset{}, nil, err
		}
		defer cf.Close()
		coms = cf
	}
	ds, preport, err := importer.ReadDataset(src, f, refs, coms)
	if err != nil {
		return ds, nil, fmt.Errorf("parse %s %w", src, err)
	}
	return ds, preport, nil
}

func printImportReport(w io.Writer, src importer.Source, r *importer.Report) {
	mode := "imported"
	switch {
	case r.DryRun:
		mode = "dry-run (nothing written)"
	case r.NothingNew:
		mode = "nothing new (nothing written)"
	}
	fmt.Fprintf(w, "%s: batch_id=%d affiliates=%d codes_created=%d code_clashes=%d warnings=%d\n",
		mode, r.BatchID, r.Affiliates, r.Codes, r.CodeClashes, len(r.Warnings))
	if r.NothingNew {
		if r.PreviousBatch > 0 {
			fmt.Fprintf(w, "This export is already imported as batch %d. Re-running it is safe and changes nothing.\n", r.PreviousBatch)
		} else {
			fmt.Fprintln(w, "Everything in this export is already here. Re-running it is safe and changes nothing.")
		}
	}
	verb := "created"
	if r.DryRun {
		verb = "would be created"
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\nCounts")
	fmt.Fprintf(tw, "  affiliates\t%d in file, %d %s, %d already here\n", r.Affiliates, r.AffiliatesCreated, verb, r.Affiliates-r.AffiliatesCreated)
	fmt.Fprintf(tw, "  links\t%d referral codes carried over, %d %s\n", r.Links, r.Codes, verb)
	fmt.Fprintf(tw, "  coupons\t%d %s\n", r.Coupons, verb)
	fmt.Fprintf(tw, "  referrals\t%d read, %d customer bindings %s\n", r.Referrals, r.Bindings, verb)
	fmt.Fprintf(tw, "  commissions\t%d read, not imported (settle balances in %s)\n", r.Commissions, src)
	for _, cur := range slices.Sorted(maps.Keys(r.CommissionTotals)) {
		fmt.Fprintf(tw, "  \t%s %s total in the export\n", cur, importer.FormatMinor(r.CommissionTotals[cur], cur))
	}
	_ = tw.Flush()

	if len(r.Conflicts) > 0 {
		fmt.Fprintln(w, "\nConflicts")
		for _, k := range r.Conflicts.Kinds() {
			it := r.Conflicts[k]
			fmt.Fprintf(w, "  %-24s %4d  %s\n", k, it.Count, importer.IssueText[k])
			if len(it.Examples) > 0 {
				fmt.Fprintf(w, "  %-24s       e.g. %s\n", "", strings.Join(it.Examples, ", "))
			}
		}
	} else {
		fmt.Fprintln(w, "\nConflicts: none")
	}

	if r.Fraud != nil {
		fmt.Fprintf(w, "\nFraud review of the %s history\n", src)
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, s := range r.Fraud.Rules {
			if s.Ran {
				line := fmt.Sprintf("  ran\t%s\t%d flagged", s.Rule, s.Hits)
				if s.Why != "" {
					line += "\t(" + s.Why + ")"
				}
				fmt.Fprintln(tw, line)
			} else {
				fmt.Fprintf(tw, "  could not run\t%s\t%s\n", s.Rule, s.Why)
			}
		}
		_ = tw.Flush()
		if len(r.Fraud.Findings) == 0 {
			fmt.Fprintln(w, "  No fraud found by the rules that ran.")
		} else {
			fmt.Fprintln(w, "  Findings (review before paying anyone)")
			for _, f := range r.Fraud.Findings {
				fmt.Fprintf(w, "    %s  %s: %s\n", f.Rule, f.Subject, f.Reason)
			}
		}
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// runCompare is side-by-side mode: the rival's latest export next to our
// data for the same days.
func runCompare(dsn string, args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	srcFlag := fs.String("from", "", "incumbent source: rewardful|firstpromoter|tapfiliate|tolt")
	refFlag := fs.String("referrals", "", "the incumbent's referrals CSV export")
	comFlag := fs.String("commissions", "", "the incumbent's commissions CSV export")
	clkFlag := fs.String("clicks", "", "the incumbent's clicks/visits CSV export")
	dateFlag := fs.String("date", "", "one day to compare, YYYY-MM-DD (default: yesterday, UTC)")
	sinceFlag := fs.String("since", "", "range start, YYYY-MM-DD (with -until)")
	untilFlag := fs.String("until", "", "range end, YYYY-MM-DD, inclusive (default: -since)")
	jsonOut := fs.Bool("json", false, "machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src, ok := importer.ParseSource(*srcFlag)
	if !ok || (*refFlag == "" && *comFlag == "" && *clkFlag == "") {
		return errors.New("usage: affiliate-engine -db <dsn> compare -from <source> [-referrals f] [-commissions f] [-clicks f] [-date YYYY-MM-DD | -since D [-until D]] [-json]")
	}
	day := func(s string) (time.Time, error) {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return t, fmt.Errorf("bad date %q (want YYYY-MM-DD)", s)
		}
		return t, nil
	}
	var in importer.CompareInput
	var err error
	switch {
	case *sinceFlag != "":
		if in.From, err = day(*sinceFlag); err != nil {
			return err
		}
		in.To = in.From
		if *untilFlag != "" {
			if in.To, err = day(*untilFlag); err != nil {
				return err
			}
		}
	case *dateFlag != "":
		if in.From, err = day(*dateFlag); err != nil {
			return err
		}
		in.To = in.From
	default:
		y := time.Now().UTC().AddDate(0, 0, -1)
		in.From = time.Date(y.Year(), y.Month(), y.Day(), 0, 0, 0, 0, time.UTC)
		in.To = in.From
	}
	open := func(p string, fn func(io.Reader) error) error {
		if p == "" {
			return nil
		}
		f, err := os.Open(p) // #nosec G304 -- operator-supplied CLI path
		if err != nil {
			return err
		}
		defer f.Close()
		return fn(f)
	}
	if err := open(*refFlag, func(r io.Reader) error {
		refs, _, err := importer.ParseReferrals(src, r)
		in.Referrals, in.HasReferrals = refs, err == nil
		return err
	}); err != nil {
		return err
	}
	if err := open(*comFlag, func(r io.Reader) error {
		coms, _, err := importer.ParseCommissions(src, r)
		in.Commissions, in.HasCommissions = coms, err == nil
		return err
	}); err != nil {
		return err
	}
	if err := open(*clkFlag, func(r io.Reader) error {
		days, _, err := importer.ParseClicks(src, r)
		in.Clicks, in.HasClicks = days, err == nil
		return err
	}); err != nil {
		return err
	}

	ctx := context.Background()
	h, closeDB, err := openImportDB(ctx, dsn)
	if err != nil {
		return fmt.Errorf("compare %w", err)
	}
	defer closeDB()
	rep, err := importer.Compare(ctx, h, src, in)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}
	printCompare(os.Stdout, rep)
	return nil
}

func printCompare(w io.Writer, r *importer.CompareReport) {
	rng := r.From
	if r.To != r.From {
		rng += " to " + r.To
	}
	fmt.Fprintf(w, "Side by side: %s vs AffiliateTracking, %s (UTC)\n\n", r.Source, rng)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "METRIC\t%s\tOURS\tDIFF\t\n", strings.ToUpper(r.Source))
	for _, row := range r.Rows {
		name := row.Metric
		fmtN := func(n int64) string { return fmt.Sprint(n) }
		if row.Currency != "" {
			name += " " + row.Currency
			cur := row.Currency
			fmtN = func(n int64) string { return importer.FormatMinor(n, cur) }
		}
		theirs, diff := "n/a", "n/a"
		if row.Theirs != nil {
			theirs = fmtN(*row.Theirs)
			diff = fmtN(*row.Diff)
			if *row.Diff > 0 {
				diff = "+" + diff
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\n", name, theirs, fmtN(row.Ours), diff)
	}
	_ = tw.Flush()
	for _, n := range r.Notes {
		fmt.Fprintf(w, "\n%s", n)
	}
	fmt.Fprintln(w)
}

// linkParams is the query param each incumbent's affiliate links use by
// default (customer.com/?via=code for Rewardful).

// printImportNextSteps tells the operator how the old links keep working:
// /r/<code> already does; links straight to their site need t.js with the
// incumbent's param.
func printImportNextSteps(w io.Writer, src importer.Source, publicURL string) {
	param := importer.LinkParam(src)
	if param == "" {
		return
	}
	origin := "https://YOUR-ENGINE-HOST"
	if u, err := url.Parse(strings.TrimRight(publicURL, "/")); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	fmt.Fprintf(w, "\nNext steps\n")
	fmt.Fprintf(w, "  Your %s links look like yoursite.com/?%s=CODE.\n", src, param)
	fmt.Fprintf(w, "  To keep them working, add this line to every page of your site:\n\n")
	fmt.Fprintf(w, "    <script src=\"%s/t.js\" data-param=\"%s\" async></script>\n\n", origin, param)
	fmt.Fprintf(w, "  Used a custom link parameter? Put its name in data-param instead.\n")
	fmt.Fprintf(w, "  Links to %s/r/CODE work already.\n", origin)
}
