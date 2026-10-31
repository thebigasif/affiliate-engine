package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// creditRow selects the commission rows the on-write aggregate counted: a
// purchase credit (bumpDaily), not a clawback (negative, with a parent)
// and not a usage settlement (never aggregated; rate_snapshot origin
// "usage"). rate_snapshot is JSON text on SQLite and jsonb on Postgres.
func credit(engine Engine, alias string) string {
	origin := "json_extract(" + alias + ".rate_snapshot, '$.origin')"
	if engine == Postgres {
		origin = alias + ".rate_snapshot->>'origin'"
	}
	return alias + ".amount > 0 AND " + alias + ".parent_commission_id IS NULL AND COALESCE(" + origin + ", '') <> 'usage'"
}

// aggregatesPerCurrency rebuilds daily_agg and token_stats_daily of a
// database written before they had a currency in their key, when every
// currency's minor units were added into one commission_cents.
//
// Clicks move to the currency-less (”) rows. Money moves to one row per
// currency:
//   - a partner (and program) whose commission rows are all in one
//     currency keeps its rows and days as they were, in that currency;
//   - a partner with commissions in several currencies has its money
//     rebuilt from its commission rows, one row per UTC day the commission
//     was recorded and currency (the old rows cannot be split);
//   - money with no commission row behind it is put in the program
//     currency.
//
// Token rows with money for a partner in several currencies cannot be
// split by dimension and are dropped (no shipped code writes token money:
// Rollup recomputes clicks and ApplyConversion had no caller).
//
// It runs in one transaction (serialized across replicas on Postgres), and
// re-runs do nothing: the column is there.
func aggregatesPerCurrency(ctx context.Context, db *sql.DB, engine Engine) (err error) {
	done, err := aggregatesDone(ctx, db, engine)
	if err != nil || done {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("per-currency aggregates: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if engine == Postgres {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('ae:agg-currency', 0))`); err != nil {
			return fmt.Errorf("per-currency aggregates: lock: %w", err)
		}
		// Another replica may have rebuilt while this one waited.
		if done, err := aggregatesDone(ctx, tx, engine); err != nil || done {
			if err == nil {
				err = tx.Commit()
			}
			return err
		}
	}
	prog := "USD"
	var c sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT currency FROM program_settings WHERE id = 1`).Scan(&c); err == nil &&
		strings.TrimSpace(c.String) != "" {
		prog = strings.ToUpper(strings.TrimSpace(c.String))
	}
	for _, t := range []aggTable{dailyAggTable(engine), tokenStatsTable(engine)} {
		if err := t.rebuild(ctx, tx, engine, prog); err != nil {
			return fmt.Errorf("per-currency aggregates: %s: %w", t.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("per-currency aggregates: commit: %w", err)
	}
	return nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// aggregatesDone reports whether both tables already have a currency.
func aggregatesDone(ctx context.Context, q querier, engine Engine) (bool, error) {
	for _, table := range []string{"daily_agg", "token_stats_daily"} {
		var n int
		var err error
		if engine == Postgres {
			err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = ? AND column_name = 'currency'`, table).Scan(&n)
		} else {
			err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'currency'`, table).Scan(&n)
		}
		if err != nil {
			return false, fmt.Errorf("per-currency aggregates: %w", err)
		}
		if n == 0 {
			return false, nil
		}
	}
	return true, nil
}

// aggTable is one aggregate table: its name, the new shape's DDL, and its
// dimension columns beyond (day, affiliate_id, campaign_id).
type aggTable struct {
	name, ddl string
	dims      []string
	// fromCommissions: a partner in several currencies gets its money
	// from the commission rows (daily_agg); token rows cannot be.
	fromCommissions bool
}

func dailyAggTable(engine Engine) aggTable {
	big, tail := "BIGINT", ""
	if engine == SQLite {
		big, tail = "INTEGER", " WITHOUT ROWID"
	}
	return aggTable{name: "daily_agg", fromCommissions: true, ddl: `CREATE TABLE daily_agg_v2 (
		day INTEGER NOT NULL, affiliate_id ` + big + ` NOT NULL, campaign_id ` + big + ` NOT NULL,
		clicks ` + big + ` NOT NULL DEFAULT 0, conversions ` + big + ` NOT NULL DEFAULT 0,
		commission_cents ` + big + ` NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
		CHECK (currency <> '' OR (conversions = 0 AND commission_cents = 0)),
		PRIMARY KEY (day, affiliate_id, campaign_id, currency))` + tail}
}

func tokenStatsTable(engine Engine) aggTable {
	id, money := "INTEGER", "INTEGER"
	if engine == Postgres {
		id, money = "BIGINT", "BIGINT"
	}
	return aggTable{name: "token_stats_daily", dims: []string{"dim", "value"}, ddl: `CREATE TABLE token_stats_daily_v2 (
		day INTEGER NOT NULL, affiliate_id ` + id + ` NOT NULL, campaign_id ` + id + ` NOT NULL,
		dim TEXT NOT NULL CHECK (dim IN ('utm_source','subid','placement')), value TEXT NOT NULL,
		clicks INTEGER NOT NULL DEFAULT 0, conversions INTEGER NOT NULL DEFAULT 0,
		commission_cents ` + money + ` NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
		CHECK (currency <> '' OR (conversions = 0 AND commission_cents = 0)),
		PRIMARY KEY (day, affiliate_id, campaign_id, dim, value, currency))`}
}

func (t aggTable) rebuild(ctx context.Context, tx *sql.Tx, engine Engine, prog string) error {
	v2 := t.name + "_v2"
	keys := "day, affiliate_id, campaign_id"
	dkeys := "d.day, d.affiliate_id, d.campaign_id"
	for _, d := range t.dims {
		keys += ", " + d
		dkeys += ", d." + d
	}
	// The one currency of each (partner, program)'s credits; n > 1 means
	// several.
	single := `SELECT affiliate_id, campaign_id, MIN(currency) AS currency, COUNT(DISTINCT currency) AS n
		FROM commissions c WHERE ` + credit(engine, "c") + ` GROUP BY affiliate_id, campaign_id`
	stmts := []struct {
		q    string
		args []any
	}{
		{`DROP TABLE IF EXISTS ` + v2, nil},
		{t.ddl, nil},
		// Clicks: currency-less.
		{`INSERT INTO ` + v2 + ` (` + keys + `, currency, clicks)
			SELECT ` + keys + `, '', clicks FROM ` + t.name + ` WHERE clicks <> 0`, nil},
		// Money of a partner in one currency (or none: the program's).
		{`INSERT INTO ` + v2 + ` (` + keys + `, currency, conversions, commission_cents)
			SELECT ` + dkeys + `, COALESCE(s.currency, ?), d.conversions, d.commission_cents
			FROM ` + t.name + ` d LEFT JOIN (` + single + `) s
			  ON s.affiliate_id = d.affiliate_id AND s.campaign_id = d.campaign_id
			WHERE (d.conversions <> 0 OR d.commission_cents <> 0) AND COALESCE(s.n, 0) <= 1`, []any{prog}},
	}
	if t.fromCommissions {
		// Money of a partner in several currencies, from its commissions.
		// Sessions run in UTC on both engines (db.Open), so the text of
		// created_at starts with its UTC date.
		stmts = append(stmts, struct {
			q    string
			args []any
		}{`INSERT INTO ` + v2 + ` (day, affiliate_id, campaign_id, currency, conversions, commission_cents)
			SELECT CAST(REPLACE(SUBSTR(CAST(c.created_at AS TEXT), 1, 10), '-', '') AS INTEGER),
			       c.affiliate_id, c.campaign_id, c.currency, COUNT(*), SUM(c.amount)
			FROM commissions c
			WHERE ` + credit(engine, "c") + `
			  AND EXISTS (SELECT 1 FROM commissions o WHERE o.affiliate_id = c.affiliate_id
			              AND o.campaign_id = c.campaign_id AND ` + credit(engine, "o") + ` AND o.currency <> c.currency)
			GROUP BY CAST(REPLACE(SUBSTR(CAST(c.created_at AS TEXT), 1, 10), '-', '') AS INTEGER),
			         c.affiliate_id, c.campaign_id, c.currency`, nil})
	}
	stmts = append(stmts, []struct {
		q    string
		args []any
	}{
		{`DROP TABLE ` + t.name, nil},
		{`ALTER TABLE ` + v2 + ` RENAME TO ` + t.name, nil},
	}...)
	if engine == Postgres {
		stmts = append(stmts, struct {
			q    string
			args []any
		}{`ALTER TABLE ` + t.name + ` RENAME CONSTRAINT ` + v2 + `_pkey TO ` + t.name + `_pkey`, nil})
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			return fmt.Errorf("%w\n%s", err, s.q)
		}
	}
	return nil
}
