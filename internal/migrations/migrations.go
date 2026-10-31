// Package migrations applies forward-only schema migrations.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed sqlite.sql pg.sql accounts_sqlite.sql accounts_pg.sql clicks_sqlite.sql clicks_pg.sql payouts_sqlite.sql payouts_pg.sql fraud_sqlite.sql fraud_pg.sql fraud2_sqlite.sql fraud2_pg.sql apikeys_sqlite.sql apikeys_pg.sql portal_sqlite.sql portal_pg.sql postbacks_sqlite.sql postbacks_pg.sql mlm_sqlite.sql mlm_pg.sql campaigns_sqlite.sql campaigns_pg.sql usage_sqlite.sql usage_pg.sql gamify_sqlite.sql gamify_pg.sql tokenstats_sqlite.sql tokenstats_pg.sql dc_sqlite.sql dc_pg.sql codes_sqlite.sql codes_pg.sql imports_sqlite.sql imports_pg.sql imports2_sqlite.sql imports2_pg.sql audit_sqlite.sql audit_pg.sql branding_sqlite.sql branding_pg.sql productrates_sqlite.sql productrates_pg.sql tiers_sqlite.sql tiers_pg.sql convlink_sqlite.sql convlink_pg.sql taxdoc_sqlite.sql taxdoc_pg.sql taxdocfile_sqlite.sql taxdocfile_pg.sql settings_sqlite.sql settings_pg.sql payoutexport_sqlite.sql payoutexport_pg.sql clickcode_sqlite.sql clickcode_pg.sql custcampaign_sqlite.sql custcampaign_pg.sql webhookhealth_sqlite.sql webhookhealth_pg.sql newsoptout_sqlite.sql newsoptout_pg.sql brandlogo_sqlite.sql brandlogo_pg.sql payoutdetails_sqlite.sql payoutdetails_pg.sql ownernotify_sqlite.sql ownernotify_pg.sql payoutask_sqlite.sql payoutask_pg.sql programmail_sqlite.sql programmail_pg.sql resources_sqlite.sql resources_pg.sql railsafety_sqlite.sql railsafety_pg.sql percurrency_sqlite.sql percurrency_pg.sql aggcurrency_sqlite.sql aggcurrency_pg.sql reversal_sqlite.sql reversal_pg.sql paymentclaims_sqlite.sql paymentclaims_pg.sql guards_sqlite.sql guards_pg.sql
var files embed.FS

// Files exposes the embedded migration scripts (for chain-validation tests).
func Files() embed.FS { return files }

// Engine names a supported database engine.
type Engine string

const (
	SQLite   Engine = "sqlite"
	Postgres Engine = "postgres"
)

// SQLiteGuardTriggers names every money/audit guard trigger that
// guards_sqlite.sql (re)creates. Restore verifies each one exists on the
// staged copy after the chain is re-applied.
var SQLiteGuardTriggers = []string{
	"trg_journal_balanced",
	"trg_ledger_entries_no_update",
	"trg_ledger_entries_no_delete",
	"trg_journal_no_update",
	"trg_journal_no_delete",
	"trg_commission_status",
	"trg_commission_money_immutable",
	"trg_commission_no_delete",
	"trg_payout_money_immutable",
	"trg_payout_status",
	"trg_payout_no_delete",
	"trg_payout_lines_no_delete",
	"trg_payout_export_once",
	"trg_audit_no_update",
	"trg_audit_no_delete",
	"trg_audit_commission_status",
	"trg_audit_payout_status",
	"trg_audit_affiliate_status",
	"trg_audit_taxdoc_status",
}

// stem is the per-engine file suffix: SQLite files are *_sqlite.sql, PG
// files are *_pg.sql.
func stem(engine Engine) string {
	if engine == Postgres {
		return "pg"
	}
	return string(engine)
}

// Names returns the ordered migration script file names for an engine —
// the exact list Apply executes. Exposed so the chain-validation test
// derives its file set from production order instead of a hand-maintained
// parallel list that can drift (fraud2/tokenstats/dc once drifted this way).
//
// guards_* is deliberately LAST: it drops and recreates every guard
// trigger, so its definitions win over any earlier CREATE ... IF NOT
// EXISTS (which never replaces an existing, possibly tampered, trigger).
func Names(engine Engine) []string {
	s := stem(engine)
	return []string{
		s + ".sql",
		"accounts_" + s + ".sql",
		"clicks_" + s + ".sql",
		"payouts_" + s + ".sql",
		"fraud_" + s + ".sql",
		"fraud2_" + s + ".sql",
		"apikeys_" + s + ".sql",
		"portal_" + s + ".sql",
		"postbacks_" + s + ".sql",
		"mlm_" + s + ".sql",
		"campaigns_" + s + ".sql",
		"usage_" + s + ".sql",
		"gamify_" + s + ".sql",
		"tokenstats_" + s + ".sql",
		"dc_" + s + ".sql",
		"codes_" + s + ".sql",
		"imports_" + s + ".sql",
		"imports2_" + s + ".sql",
		"audit_" + s + ".sql",
		"branding_" + s + ".sql",
		"productrates_" + s + ".sql",
		"tiers_" + s + ".sql",
		"convlink_" + s + ".sql",
		"taxdoc_" + s + ".sql",
		"taxdocfile_" + s + ".sql",
		"settings_" + s + ".sql",
		"payoutexport_" + s + ".sql",
		"clickcode_" + s + ".sql",
		"custcampaign_" + s + ".sql",
		"webhookhealth_" + s + ".sql",
		"newsoptout_" + s + ".sql",
		"brandlogo_" + s + ".sql",
		"payoutdetails_" + s + ".sql",
		"ownernotify_" + s + ".sql",
		"payoutask_" + s + ".sql",
		"programmail_" + s + ".sql",
		"resources_" + s + ".sql",
		"railsafety_" + s + ".sql",
		"percurrency_" + s + ".sql",
		"aggcurrency_" + s + ".sql",
		"reversal_" + s + ".sql",
		"paymentclaims_" + s + ".sql",
		"guards_" + s + ".sql",
	}
}

// steps are Go migration steps, run right after the named script, for
// work SQL alone cannot do safely. Each must be idempotent: it runs on
// every Apply, like the scripts.
var steps = map[string]func(ctx context.Context, db *sql.DB, engine Engine) error{
	"percurrency_sqlite.sql": rehomeLedger,
	"percurrency_pg.sql":     rehomeLedger,
	"aggcurrency_sqlite.sql": aggregatesPerCurrency,
	"aggcurrency_pg.sql":     aggregatesPerCurrency,
}

// Apply runs the whole chain for the engine, one Exec per file. It runs on
// every start (and every CLI self-migrate): the embedded SQL is idempotent
// (IF NOT EXISTS everywhere), and the final guards script re-asserts every
// guard trigger each time.
func Apply(ctx context.Context, db *sql.DB, engine Engine) error {
	for _, name := range Names(engine) {
		if err := ApplyFile(ctx, db, engine, name); err != nil {
			return err
		}
	}
	return nil
}

// Script returns one embedded chain script (a name from Names).
// Callers that track what they applied hash it.
func Script(name string) ([]byte, error) {
	b, err := files.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("migrations: missing %s: %w", name, err)
	}
	return b, nil
}

// ApplyFile runs one script of the chain and its Go step, exactly as
// Apply does for that file. Apply is the loop over Names; a caller that
// records applied files per database (the Shopify app's shop schemas,
// internal/shopify/tenancy) calls this for the files it has not run.
func ApplyFile(ctx context.Context, db *sql.DB, engine Engine, name string) error {
	script, err := Script(name)
	if err != nil {
		return err
	}
	sqlText := string(script)
	if engine == SQLite {
		if sqlText, err = dropPresentColumns(ctx, db, sqlText); err != nil {
			return fmt.Errorf("migrations: apply %s: %w", name, err)
		}
	}
	if _, err := db.ExecContext(ctx, sqlText); err != nil {
		// Idempotent-ALTER convention: SQLite has no ADD COLUMN IF NOT
		// EXISTS, so re-running a script whose columns already exist
		// reports "duplicate column name". PG's IF NOT EXISTS never
		// trips this. Column-already-present is success, not failure.
		if !isDuplicateColumn(err) {
			return fmt.Errorf("migrations: apply %s: %w", name, err)
		}
	}
	if step := steps[name]; step != nil {
		if err := step(ctx, db, engine); err != nil {
			return fmt.Errorf("migrations: after %s: %w", name, err)
		}
	}
	return nil
}

// sqliteAddColumn matches one single-line SQLite ADD COLUMN statement.
var sqliteAddColumn = regexp.MustCompile(`(?im)^[ \t]*ALTER[ \t]+TABLE[ \t]+(\w+)[ \t]+ADD[ \t]+COLUMN[ \t]+(\w+)[^;\n]*;`)

// dropPresentColumns removes the ADD COLUMN statements whose column already
// exists. SQLite has no ADD COLUMN IF NOT EXISTS, and a multi-statement
// Exec stops at its first error: a file whose first ALTER was applied by
// an older release used to abort there, so a column added later in the
// same file (campaigns.commission_months) never reached upgraded
// databases. Filtering first makes every file safe to extend.
func dropPresentColumns(ctx context.Context, db *sql.DB, script string) (string, error) {
	var ferr error
	out := sqliteAddColumn.ReplaceAllStringFunc(script, func(stmt string) string {
		if ferr != nil {
			return stmt
		}
		m := sqliteAddColumn.FindStringSubmatch(stmt)
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m[1], m[2]).Scan(&n); err != nil {
			ferr = err
			return stmt
		}
		if n > 0 {
			return "-- (already present) " + strings.TrimSpace(stmt)
		}
		return stmt
	})
	return out, ferr
}

// isDuplicateColumn matches SQLite's "duplicate column name" (PG uses IF
// NOT EXISTS and never reaches this).
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}
