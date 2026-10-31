// dispatch.go — subcommand resolution. The first POSITIONAL argument
// (flag.Args()[0], never raw os.Args) names the subcommand; an unknown
// word is a hard error, not a silent server start. Rationale: the old
// dispatch scanned os.Args[1:] and fell through to serve mode on any
// unmatched word — a typo (`dctor`) or `help` silently started a server,
// and a flag VALUE (`-name backup`) could shadow-trigger a subcommand.
package main

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

// cliCommands is the single source of truth for the subcommand surface:
// printUsage lists it, dispatchCLI's switch must wire every key (the
// default branch fails loudly on a declared-but-unwired entry).
var cliCommands = map[string]string{
	"backup":          "snapshot the sqlite db (VACUUM INTO): [out.db] (sqlite only)",
	"doctor":          "integrity report: ledger sum-0, payable vs commissions, drift, backlog (sqlite only)",
	"restore":         "restore -from <backup.db> [--force] — verify + atomic swap (sqlite only)",
	"migrate":         "apply the migration chain to -db (sqlite: or postgres://)",
	"keygen":          "keygen -scope admin | -scope affiliate -affiliate N [-name X]",
	"import":          "import an incumbent's CSV exports: -from S -csv F [-referrals F] [-commissions F] [-dry-run] | -undo N | -list",
	"compare":         "side by side with an incumbent: -from S [-referrals F] [-commissions F] [-clicks F] [-date D]",
	"bench":           "self-contained perf gates on a throwaway db",
	"export-parquet":  "clicks + daily_agg -> parquet for a DuckDB sidecar (-out dir)",
	"gdpr-export":     "subject data export: -affiliate N [-out file.json]",
	"gdpr-erase":      "right-to-erasure: -affiliate N -yes (scrubs identity, keeps money rows)",
	"screen-backfill": "fraud-screen commissions older releases never screened: [-dry-run] [-limit N] [-since YYYY-MM-DD] [-json]",
	"test-conversion": "simulate a conversion end-to-end against -db (sqlite only)",
	"demo-seed":       "build the live demo's sample program: -db sqlite:<path> [-reset] (sqlite only)",
	"help":            "show subcommand usage",
	"version":         "print the build version",
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "affiliate-engine — self-hostable affiliate tracking, fraud-first")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: affiliate-engine [-db dsn] [-listen addr] [subcommand [args]]")
	fmt.Fprintln(w, "       (global flags come BEFORE the subcommand; no subcommand = serve)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	for _, k := range slices.Sorted(maps.Keys(cliCommands)) {
		fmt.Fprintf(w, "  %-16s %s\n", k, cliCommands[k])
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Env: AE_TOKEN_SECRET, AE_SECRET_FILE, AE_ALLOWED_DEST, AE_STRIPE_SECRET, AE_PADDLE_SECRET,")
	fmt.Fprintln(w, "     AE_CHARGEBEE_USERNAME, AE_CHARGEBEE_PASSWORD, AE_CHARGEBEE_SECRET, AE_LEMONSQUEEZY_SECRET, AE_POLAR_SECRET,")
	fmt.Fprintln(w, "     AE_RECURLY_USERNAME, AE_RECURLY_PASSWORD, AE_RECURLY_SIGNATURE_SECRET, AE_STRIPE_SECRET_KEY,")
	fmt.Fprintln(w, "     AE_STRIPE_PRODUCTS, AE_STRIPE_API_KEY, AE_MAIL_PROVIDER, AE_MAIL_KEY, AE_MAIL_FROM, AE_MAIL_REPLY_TO, AE_OWNER_EMAIL,")
	fmt.Fprintln(w, "     AE_PUBLIC_URL, AE_TRUSTED_PROXIES, AE_REDIRECT_RATE_PER_MIN, AE_MIGRATE_DATABASE_URL,")
	fmt.Fprintln(w, "     AE_FRAUD_DRYRUN, AE_IP_MODE, AE_CLICK_RETENTION_DAYS, AE_COOKIE_SECURE, AE_DEMO — see README.")
}

// dispatchCLI resolves and runs the subcommand named by rest (the
// positional args left after global flag parsing). serve=true means no
// subcommand was given: the caller falls through to server mode.
// Otherwise exitCode is the process status the caller must exit with —
// dispatchCLI itself never os.Exits so it stays unit-testable.
func dispatchCLI(rest []string, dsn string) (serve bool, exitCode int) {
	if len(rest) == 0 {
		return true, 0
	}
	sub, subArgs := rest[0], rest[1:]
	switch sub {
	case "help":
		printUsage(os.Stdout)
		return false, 0
	case "version":
		fmt.Println(version)
		return false, 0
	case "backup":
		if err := requireSQLite(sub, dsn); err != nil {
			return exitWrap(err)
		}
		out := ""
		if len(subArgs) > 0 {
			out = subArgs[0]
		}
		return exitWrap(runBackup(sqlitePath(dsn), out))
	case "doctor":
		if err := requireSQLite(sub, dsn); err != nil {
			return exitWrap(err)
		}
		return exitWrap(runDoctor(sqlitePath(dsn)))
	case "migrate":
		if dsn == "" {
			fmt.Fprintln(os.Stderr, "affiliate-engine: migrate requires -db")
			return false, 2
		}
		return exitWrap(runMigrate(dsn))
	case "keygen":
		return exitWrap(runKeygen(dsn, subArgs))
	case "restore":
		if err := requireSQLite(sub, dsn); err != nil {
			return exitWrap(err)
		}
		rfs := flag.NewFlagSet("restore", flag.ContinueOnError)
		from := rfs.String("from", "", "backup file (a VACUUM INTO output)")
		force := rfs.Bool("force", false, "allow restoring when no live db exists")
		if err := rfs.Parse(subArgs); err != nil {
			fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
			return false, 2
		}
		if dsn == "" || strings.TrimPrefix(dsn, "sqlite:") == "" {
			fmt.Fprintln(os.Stderr, "affiliate-engine: restore requires -db sqlite:<path>")
			return false, 2
		}
		return exitWrap(runRestore(*from, sqlitePath(dsn), *force))
	case "import":
		target := dsn
		if target == "" {
			target = "sqlite:affiliate-engine.db"
		}
		return exitWrap(runImport(target, subArgs))
	case "compare":
		target := dsn
		if target == "" {
			target = "sqlite:affiliate-engine.db"
		}
		return exitWrap(runCompare(target, subArgs))
	case "bench":
		// Perf gates run against a throwaway temp db — never a real install.
		return exitWrap(runBench(subArgs))
	case "export-parquet":
		target := dsn
		if target == "" {
			target = "sqlite:affiliate-engine.db"
		}
		return exitWrap(runExportParquet(target, subArgs))
	case "gdpr-export":
		target := dsn
		if target == "" {
			target = "sqlite:affiliate-engine.db"
		}
		return exitWrap(runGDPRExport(target, subArgs))
	case "gdpr-erase":
		target := dsn
		if target == "" {
			target = "sqlite:affiliate-engine.db"
		}
		return exitWrap(runGDPRErase(target, subArgs))
	case "screen-backfill":
		target := dsn
		if target == "" {
			target = "sqlite:affiliate-engine.db"
		}
		return exitWrap(runScreenBackfill(target, subArgs))
	case "test-conversion":
		if err := requireSQLite(sub, dsn); err != nil {
			return exitWrap(err)
		}
		return exitWrap(runTestConversion(sqlitePath(dsn), subArgs))
	case "demo-seed":
		if err := requireSQLite(sub, dsn); err != nil {
			return exitWrap(err)
		}
		return exitWrap(runDemoSeed(dsn, subArgs))
	default:
		if _, declared := cliCommands[sub]; declared {
			// Map entry without a switch case — must never be a silent no-op.
			fmt.Fprintf(os.Stderr, "affiliate-engine: %q is declared but not wired (bug)\n", sub)
			return false, 3
		}
		fmt.Fprintf(os.Stderr, "affiliate-engine: unknown subcommand %q\n\n", sub)
		printUsage(os.Stderr)
		return false, 2
	}
}

// exitWrap converts a subcommand error into (don't serve, exit 1);
// success is exit 0. The error message is printed here so every case
// shares one convention.
func exitWrap(err error) (bool, int) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "affiliate-engine: %v\n", err)
		return false, 1
	}
	return false, 0
}

// isPostgresDSN reports whether dsn selects the Postgres engine.
func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres:") || strings.HasPrefix(dsn, "postgresql:")
}

// requireSQLite refuses file-level commands on a Postgres DSN. Before this
// guard they ran sqlitePath() on the DSN and silently CREATED a SQLite
// file named after the connection string (credentials included).
func requireSQLite(cmd, dsn string) error {
	if isPostgresDSN(dsn) {
		return fmt.Errorf("%s: not supported on postgres — run the equivalent against the server with psql/pg_dump", cmd)
	}
	return nil
}

// sqlitePath derives the sqlite file path from the -db DSN, applying the
// documented default when unset.
func sqlitePath(dsn string) string {
	p := strings.TrimPrefix(dsn, "sqlite:")
	if p == "" {
		return "affiliate-engine.db"
	}
	return p
}
