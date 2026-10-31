// Package analytics — parquet export (FR-26): ad-hoc analytics by
// exporting clicks + daily_agg to parquet for a DuckDB read-only sidecar.
//
// Design notes:
//   - parquet-go is pure Go (CGO-free — NFR-4/G5 constraint) and streams
//     rows in bounded chunks; never all-in-RAM.
//   - The export is a READ-ONLY snapshot of the analytics tables; money
//     tables have their own audited surfaces and are out of scope here.
//   - ip_hash/ua_hash travel as opaque 16-byte pseudonyms (the same
//     non-reversible identity hashes the product already stores; they are
//     what makes velocity/fraud pattern analysis in DuckDB possible).
package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/parquet-go/parquet-go"
)

// ClickRow is the parquet schema for one raw click. Column names match
// the clicks table (snake_case) so DuckDB queries read naturally.
type ClickRow struct {
	ID           string `parquet:"id"`
	Ts           string `parquet:"ts"`
	Day          int64  `parquet:"day"`
	Month        int64  `parquet:"month"`
	AffiliateID  int64  `parquet:"affiliate_id"`
	CampaignID   int64  `parquet:"campaign_id"`
	IPHash       []byte `parquet:"ip_hash,optional"`
	UAHash       []byte `parquet:"ua_hash,optional"`
	Referer      string `parquet:"referer"`
	LandingPath  string `parquet:"landing_path"`
	UTMSource    string `parquet:"utm_source"`
	SubID        string `parquet:"subid"`
	Placement    string `parquet:"placement"`
	Country      string `parquet:"country"`
	BotClass     int32  `parquet:"bot_class"`
	ExcludeStats int32  `parquet:"exclude_stats"`
}

// AggRow is the parquet schema for one daily_agg cell. Money is per
// currency: commission_cents is in minor units of currency (sum it per
// currency, never across). Click rows have currency "".
type AggRow struct {
	Day             int64  `parquet:"day"`
	AffiliateID     int64  `parquet:"affiliate_id"`
	CampaignID      int64  `parquet:"campaign_id"`
	Clicks          int64  `parquet:"clicks"`
	Conversions     int64  `parquet:"conversions"`
	CommissionCents int64  `parquet:"commission_cents"`
	Currency        string `parquet:"currency"`
}

// queryer is satisfied by *sql.DB and *sql.Conn on both engines.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ExportParquet writes <dir>/clicks.parquet and <dir>/daily_agg.parquet.
// fromDay/toDay bound the clicks export as YYYYMMDD integers (0 = open
// end); daily_agg always exports in full (it is the permanent aggregate —
// tiny by construction). Returns rows written per file.
func ExportParquet(ctx context.Context, q queryer, dir string, fromDay, toDay int64) (clicks, aggs int64, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, 0, fmt.Errorf("parquet: mkdir: %w", err)
	}

	clicks, err = exportClicks(ctx, q, filepath.Join(dir, "clicks.parquet"), fromDay, toDay)
	if err != nil {
		return 0, 0, err
	}
	aggs, err = exportAggs(ctx, q, filepath.Join(dir, "daily_agg.parquet"))
	if err != nil {
		return 0, 0, err
	}
	return clicks, aggs, nil
}

const exportChunk = 1024 // rows buffered per Write call (bounded memory)

func exportClicks(ctx context.Context, q queryer, path string, fromDay, toDay int64) (int64, error) {
	query := `SELECT id, ts, day, month, affiliate_id, campaign_id,
		ip_hash, ua_hash, referer, landing_path, utm_source, subid,
		placement, country, bot_class, exclude_stats
		FROM clicks`
	var args []any
	switch {
	case fromDay > 0 && toDay > 0:
		query += ` WHERE day >= ? AND day <= ?`
		args = append(args, fromDay, toDay)
	case fromDay > 0:
		query += ` WHERE day >= ?`
		args = append(args, fromDay)
	case toDay > 0:
		query += ` WHERE day <= ?`
		args = append(args, toDay)
	}
	query += ` ORDER BY day, id`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("parquet: clicks query: %w", err)
	}
	defer rows.Close()

	n, err := writeParquet(path, rows, func(rows *sql.Rows, out *[]ClickRow) error {
		var r ClickRow
		if err := rows.Scan(&r.ID, &r.Ts, &r.Day, &r.Month, &r.AffiliateID, &r.CampaignID,
			&r.IPHash, &r.UAHash, &r.Referer, &r.LandingPath, &r.UTMSource, &r.SubID,
			&r.Placement, &r.Country, &r.BotClass, &r.ExcludeStats); err != nil {
			return err
		}
		*out = append(*out, r)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("parquet: clicks: %w", err)
	}
	return n, nil
}

func exportAggs(ctx context.Context, q queryer, path string) (int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT day, affiliate_id, campaign_id,
		clicks, conversions, commission_cents, currency FROM daily_agg
		ORDER BY day, affiliate_id, campaign_id, currency`)
	if err != nil {
		return 0, fmt.Errorf("parquet: daily_agg query: %w", err)
	}
	defer rows.Close()

	n, err := writeParquet(path, rows, func(rows *sql.Rows, out *[]AggRow) error {
		var r AggRow
		if err := rows.Scan(&r.Day, &r.AffiliateID, &r.CampaignID,
			&r.Clicks, &r.Conversions, &r.CommissionCents, &r.Currency); err != nil {
			return err
		}
		*out = append(*out, r)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("parquet: daily_agg: %w", err)
	}
	return n, nil
}

// writeParquet streams rows through a chunked GenericWriter. scan fills
// one typed row per SQL row; the chunk buffer bounds memory regardless
// of table size.
func writeParquet[T any](path string, rows *sql.Rows, scan func(*sql.Rows, *[]T) error) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- path is filepath.Join(operatorDir, "clicks|daily_agg.parquet") from the export-parquet CLI; the command exists to write that exact file
	if err != nil {
		return 0, err
	}
	defer f.Close()

	w := parquet.NewGenericWriter[T](f, parquet.PageBufferSize(64*1024))
	chunk := make([]T, 0, exportChunk)
	var total int64
	for rows.Next() {
		if err := scan(rows, &chunk); err != nil {
			return 0, err
		}
		if len(chunk) == exportChunk {
			if _, err := w.Write(chunk); err != nil {
				return 0, err
			}
			total += int64(len(chunk))
			chunk = chunk[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(chunk) > 0 {
		if _, err := w.Write(chunk); err != nil {
			return 0, err
		}
		total += int64(len(chunk))
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	return total, nil
}

// ReadParquetClicks and ReadParquetAggs are the test/verification
// readers (roundtrip). They are exported so a CLI verify mode can reuse
// them; reading parquet is not part of the serving path.
func ReadParquetClicks(path string) ([]ClickRow, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("parquet: %s: %w", path, fs.ErrInvalid)
	}
	f, err := os.Open(path) // #nosec G304 -- read-back of a file this package wrote moments earlier (roundtrip/verify); path is caller-internal, never request input
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := parquet.NewGenericReader[ClickRow](f)
	out := make([]ClickRow, 0, st.Size()/64)
	buf := make([]ClickRow, exportChunk)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return out, r.Close()
}

// ReadParquetAggs reads daily_agg.parquet back (test/verify path).
func ReadParquetAggs(path string) ([]AggRow, error) {
	f, err := os.Open(path) // #nosec G304 -- read-back of a file this package wrote moments earlier (roundtrip/verify); path is caller-internal, never request input
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := parquet.NewGenericReader[AggRow](f)
	out := make([]AggRow, 0, 64)
	buf := make([]AggRow, exportChunk)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return out, r.Close()
}
