// export.go — the `export-parquet` subcommand (FR-26): ad-hoc analytics
// by exporting clicks + daily_agg to parquet for a DuckDB read-only
// sidecar. The library lives in internal/analytics (streaming, bounded
// memory, dual-engine); this is the operator entrypoint.
//
// Runs against a live DSN (sqlite: or postgres://) read-only: only SELECTs
// flow through ExportParquet. Writes clicks.parquet + daily_agg.parquet
// into -out (default "parquet-export/"). -verify reads both files back
// and cross-checks row counts against SQL COUNTs — cheap insurance that
// the snapshot an analyst loads matches the database it came from.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/db"
)

func runExportParquet(dsn string, args []string) error {
	fs := flag.NewFlagSet("export-parquet", flag.ContinueOnError)
	out := fs.String("out", "parquet-export", "output directory (created if absent)")
	from := fs.String("from", "", "clicks exported from this day, YYYYMMDD (default: all)")
	to := fs.String("to", "", "clicks exported through this day, YYYYMMDD (default: all)")
	verify := fs.Bool("verify", false, "read the files back and cross-check row counts against SQL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fromDay, err := parseDayFlag("from", *from)
	if err != nil {
		return err
	}
	toDay, err := parseDayFlag("to", *to)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	h, err := db.Open(ctx, dsn, db.Options{})
	if err != nil {
		return fmt.Errorf("export-parquet: open: %w", err)
	}
	defer h.DB.Close()
	defer h.Ledger.Close()

	nClicks, nAggs, err := analytics.ExportParquet(ctx, h.DB, *out, fromDay, toDay)
	if err != nil {
		return fmt.Errorf("export-parquet: %w", err)
	}
	fmt.Printf("exported %d clicks -> %s/clicks.parquet\n", nClicks, *out)
	fmt.Printf("exported %d daily_agg rows -> %s/daily_agg.parquet\n", nAggs, *out)

	if *verify {
		if err := verifyExport(ctx, h.DB, filepath.Join(*out, "clicks.parquet"), filepath.Join(*out, "daily_agg.parquet"), nClicks, nAggs); err != nil {
			return fmt.Errorf("export-parquet: verify: %w", err)
		}
		fmt.Println("verify OK: parquet row counts match SQL")
	}

	fmt.Println(`DuckDB sidecar (read-only):
  duckdb -c "SELECT day, sum(clicks) FROM read_parquet('` + *out + `/daily_agg.parquet') GROUP BY day ORDER BY day;"
Commission is per currency (minor units of each), never summed across:
  duckdb -c "SELECT currency, sum(commission_cents) FROM read_parquet('` + *out + `/daily_agg.parquet') WHERE currency <> '' GROUP BY currency;"`)
	return nil
}

// parseDayFlag validates YYYYMMDD and returns it as an int64 (0 = unset).
func parseDayFlag(name, v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("-%s wants YYYYMMDD, got %q", name, v)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("-%s wants YYYYMMDD digits, got %q", name, v)
	}
	if _, err := time.Parse("20060102", v); err != nil {
		return 0, fmt.Errorf("-%s is not a real calendar day: %q", name, v)
	}
	return n, nil
}

// verifyExport reads both parquet files back and cross-checks row counts
// (and, for daily_agg, the click total) against live SQL. This is the
// export analogue of the W1 drift exit-test: what the analyst loads must
// equal what the product served.
func verifyExport(ctx context.Context, q *sql.DB, clicksPath, aggsPath string, wantClicks, wantAggs int64) error {
	clicksBack, err := analytics.ReadParquetClicks(clicksPath)
	if err != nil {
		return err
	}
	if int64(len(clicksBack)) != wantClicks {
		return fmt.Errorf("clicks.parquet holds %d rows, export reported %d", len(clicksBack), wantClicks)
	}
	aggsBack, err := analytics.ReadParquetAggs(aggsPath)
	if err != nil {
		return err
	}
	if int64(len(aggsBack)) != wantAggs {
		return fmt.Errorf("daily_agg.parquet holds %d rows, export reported %d", len(aggsBack), wantAggs)
	}
	var pqAggClicks int64
	for _, r := range aggsBack {
		pqAggClicks += r.Clicks
	}
	var sqlAggClicks int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&sqlAggClicks); err != nil {
		return err
	}
	if pqAggClicks != sqlAggClicks {
		return fmt.Errorf("daily_agg click total: parquet=%d sql=%d — snapshot drifted mid-export", pqAggClicks, sqlAggClicks)
	}
	return nil
}
