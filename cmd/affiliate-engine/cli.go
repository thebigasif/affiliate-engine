// CLI subcommands: backup (VACUUM INTO) and doctor (integrity report).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
	"github.com/thebigasif/affiliate-engine/internal/money"
)

func runBackup(dbPath, outPath string) error {
	if outPath == "" {
		outPath = fmt.Sprintf("%s.backup-%s.db", strings.TrimSuffix(dbPath, ".db"),
			time.Now().UTC().Format("20060102-150405"))
	}
	// VACUUM INTO is the measured-safe compaction copy (research/data-layer/sqlite-ops.md §6).
	d, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.Exec(`VACUUM INTO ?`, outPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	// SQLite fsyncs the backup's pages on commit, but the new file's
	// directory entry is not guaranteed durable until the containing
	// directory is fsynced — a power cut could otherwise corrupt the
	// very artifact disaster recovery depends on. Loud warning, not
	// fatal: the pages themselves are already flushed.
	if err := fsyncDir(outPath); err != nil {
		fmt.Printf("backup: WARNING — could not fsync directory (%v); the backup file's directory entry is not yet durable\n", err)
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return err
	}
	fmt.Printf("backup: %s (%d bytes)\n", outPath, st.Size())
	return nil
}

// currencySums runs a (currency, sum) GROUP BY query into a map.
func currencySums(ctx context.Context, d *sql.DB, q string) (map[string]int64, error) {
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] += v
	}
	return out, rows.Err()
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func runDoctor(dbPath string) error {
	ctx := context.Background()
	h, err := openForCLI(ctx, dbPath)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	ok, err := doctor(ctx, h, os.Stdout)
	if err != nil {
		return err
	}
	if ok {
		fmt.Println("doctor: ALL CHECKS PASSED")
	} else {
		fmt.Println("doctor: FAILURES DETECTED")
		os.Exit(1)
	}
	return nil
}

// doctor runs the integrity checks, printing one line per check to w.
// ok=false means a money invariant failed. Split from runDoctor (which
// exits the process) so the demo seed and tests can run the same checks.
func doctor(ctx context.Context, h *db.Handles, w io.Writer) (ok bool, err error) {
	poster := ledger.NewPoster(h.Ledger)
	ok = true

	// 1) Ledger integrity, per currency: the entries of each currency must
	// sum to 0 (money is never netted across currencies, so a global zero
	// could hide a +X USD / -X JPY pair).
	sums, err := currencySums(ctx, h.DB, `
		SELECT COALESCE(la.currency, '?'), COALESCE(SUM(le.amount), 0)
		FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		GROUP BY la.currency`)
	if err != nil {
		return false, err
	}
	if len(sums) == 0 {
		sums[money.Default] = 0
	}
	for _, cur := range sortedKeys(sums) {
		fmt.Fprintf(w, "ledger integrity %s (sum of entries): %d — %s\n", cur, sums[cur], pass(sums[cur] == 0))
		if sums[cur] != 0 {
			ok = false
		}
	}
	if sum, err := poster.GlobalIntegrity(ctx); err != nil {
		return false, err
	} else if sum != 0 {
		ok = false
	}

	// 2) Journal balance invariant: every journal individually zero, in
	// one currency, with no line on a legacy currency-less account.
	var unbalanced, mixed, legacy int
	if err := h.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT journal_id, SUM(amount) s FROM ledger_entries GROUP BY journal_id
		) j WHERE s != 0`).Scan(&unbalanced); err != nil {
		return false, err
	}
	if err := h.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT le.journal_id FROM ledger_entries le
			JOIN ledger_accounts la ON la.id = le.account_id
			GROUP BY le.journal_id HAVING COUNT(DISTINCT COALESCE(la.currency, '')) > 1
		) j`).Scan(&mixed); err != nil {
		return false, err
	}
	if err := h.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.currency IS NULL`).Scan(&legacy); err != nil {
		return false, err
	}
	fmt.Fprintf(w, "unbalanced journals: %d, mixed-currency journals: %d, lines on legacy accounts: %d — %s\n",
		unbalanced, mixed, legacy, pass(unbalanced+mixed+legacy == 0))
	if unbalanced+mixed+legacy > 0 {
		ok = false
	}

	// 3) Commission state vs ledger, per currency: the payables must hold
	// exactly what the commissions say is owed — the earnings endpoint's
	// ledger proof (owedFromCommissions) summed over every affiliate. The
	// old check compared against approved+paid, which failed on any
	// install that had paid a payout (settled money leaves the payable; its
	// rows stay 'paid') or clawed a refund from an approved commission.
	owed, err := currencySums(ctx, h.DB, `
		SELECT c.currency, COALESCE(SUM(c.amount), 0)
		FROM commissions c
		LEFT JOIN commissions p ON p.id = c.parent_commission_id
		WHERE NOT EXISTS (SELECT 1 FROM payout_lines pl
		                  JOIN payouts po ON po.id = pl.payout_id
		                  WHERE pl.commission_id = c.id AND po.status = 'paid')
		  AND ((c.parent_commission_id IS NULL AND c.status = 'approved')
		    OR (c.parent_commission_id IS NOT NULL AND c.amount < 0
		        AND p.status IN ('approved','paid','clawback')))
		GROUP BY c.currency`)
	if err != nil {
		return false, err
	}
	payable, err := currencySums(ctx, h.DB, `
		SELECT COALESCE(la.currency, '?'), COALESCE(SUM(le.amount), 0) FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.kind = 'liability' AND la.code LIKE 'AFFILIATE_PAYABLE:%'
		GROUP BY la.currency`)
	if err != nil {
		return false, err
	}
	all := map[string]int64{money.Default: 0}
	for c := range owed {
		all[c] = 0
	}
	for c := range payable {
		all[c] = 0
	}
	for _, cur := range sortedKeys(all) {
		fmt.Fprintf(w, "payable ledger %s=%d vs owed by commissions=%d — %s\n",
			cur, -payable[cur], owed[cur], pass(-payable[cur] == owed[cur]))
		if -payable[cur] != owed[cur] {
			ok = false
		}
	}

	// 4) Clicks vs aggregates drift (yesterday).
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("20060102")
	var aggClicks, rawClicks int64
	if err := h.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(clicks),0) FROM daily_agg WHERE day=?`, yesterday).Scan(&aggClicks); err != nil {
		return false, fmt.Errorf("doctor: agg clicks: %w", err)
	}
	if err := h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM clicks WHERE day=? AND exclude_stats=0`, yesterday).Scan(&rawClicks); err != nil {
		return false, fmt.Errorf("doctor: raw clicks: %w", err)
	}
	fmt.Fprintf(w, "yesterday agg=%d raw=%d — %s\n", aggClicks, rawClicks, pass(aggClicks == rawClicks))

	// 5) Undelivered outbox backlog.
	var backlog int
	if err := h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_events WHERE delivered_at IS NULL`).Scan(&backlog); err != nil {
		return false, fmt.Errorf("doctor: outbox backlog: %w", err)
	}
	fmt.Fprintf(w, "outbox backlog: %d — %s\n", backlog, pass(backlog < 1000))

	// 6) Unresolved fraud holds.
	var holds int
	if err := h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&holds); err != nil {
		return false, fmt.Errorf("doctor: fraud holds: %w", err)
	}
	fmt.Fprintf(w, "open fraud holds: %d (review queue)\n", holds)
	return ok, nil
}

func pass(b bool) string {
	if b {
		return "OK"
	}
	return "FAIL"
}

func openForCLI(ctx context.Context, dbPath string) (*db.Handles, error) {
	h, err := db.Open(ctx, "sqlite:"+dbPath, db.Options{LedgerSyncFull: true})
	if err != nil {
		return nil, err
	}
	// CLI tools self-migrate (idempotent) so doctor/backup work on any state.
	if err := migrations.Apply(ctx, h.DB, migrations.SQLite); err != nil {
		return nil, err
	}
	return h, nil
}

// runKeygen creates a scoped API key on either engine:
//
//	affiliate-engine keygen -scope admin -name ops
//	affiliate-engine keygen -scope affiliate -affiliate 7
//
// args are the os.Args entries AFTER the "keygen" word (the global flag
// parser stops at the subcommand, so flags are parsed from a local set).
func runKeygen(dsn string, args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	scope := fs.String("scope", "", "admin | affiliate")
	affID := fs.Int64("affiliate", 0, "affiliate id (scope=affiliate)")
	name := fs.String("name", "", "key label")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var scopeStr string
	switch {
	case *scope == "admin":
		scopeStr = apikeys.ScopeAdmin
	case *scope == "affiliate" && *affID > 0:
		scopeStr = apikeys.AffiliateScope(*affID)
	default:
		return fmt.Errorf("keygen: pass -scope admin or -scope affiliate -affiliate <id>")
	}
	if *name == "" {
		*name = scopeStr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// openDSN (gdpr.go) opens + self-migrates on the DSN's own engine.
	h, err := openDSN(ctx, dsn)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	key, err := apikeys.Store(ctx, h.DB, *name, scopeStr)
	if err != nil {
		return err
	}
	fmt.Printf("API key created (scope=%s, name=%q)\n", scopeStr, *name)
	fmt.Printf("  %s\n", key)
	fmt.Println("Store it now — the key is hashed at rest and cannot be shown again.")
	return nil
}

// runMigrate applies the full migration chain to a SQLite or PostgreSQL
// DSN. Used by CI (as the source of truth for the PG chain — no
// hand-maintained cat-lists to drift) and by operators upgrading installs.
func runMigrate(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	engine := migrations.SQLite
	if isPostgresDSN(dsn) {
		engine = migrations.Postgres
	}
	h, err := db.Open(ctx, dsn, db.Options{})
	if err != nil {
		return err
	}
	defer h.DB.Close()
	if err := migrations.Apply(ctx, h.DB, engine); err != nil {
		return err
	}
	fmt.Printf("migrate: chain applied (%s)\n", engine)
	return nil
}
