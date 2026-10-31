package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// rehomeLedger moves every legacy, currency-less ledger account's lines to
// the per-currency account of their journal's currency, and removes the
// emptied legacy accounts. Earlier releases kept one HOLD, one
// EXPENSE:COMMISSION, one PAYOUT_CLEARING and one AFFILIATE_PAYABLE:<id>
// for every currency, so a yen receivable and a dollar payable netted into
// one meaningless balance.
//
// Each journal's currency comes from the money row it was posted for: its
// commission (credit, clawback and usage journals carry journal_id; the
// mature:/fraudclaw: keys and their -revert twins name the commission), its
// payout (payout:<id>), or for a credit/clawback journal whose row an older
// release never wrote, the currency of the raw event it was posted for.
// usage: journals are USD (usage settlement has only ever booked USD). A
// journal none of that attributes is attributed only when the install has
// booked a single currency (there is no other currency it can be in);
// otherwise the migration fails, naming the journals, and changes nothing.
//
// Lines move by UPDATE of ledger_entries.account_id with the append-only
// guard lifted inside the transaction: amounts, journals and ids are kept,
// so every journal still sums to zero and history stays where it was. The
// whole move commits in ONE transaction (serialized across replicas on
// Postgres), and re-runs are a no-op: no legacy account is left.
//
// On the first run the program currency (program_settings.currency) is set
// to the install's one commission currency, if it has exactly one, so a
// yen-only install's fixed rules and tiers keep meaning yen.
func rehomeLedger(ctx context.Context, db *sql.DB, engine Engine) (err error) {
	// Fast path, outside any transaction: nothing legacy left.
	var legacy int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_accounts WHERE currency IS NULL`).Scan(&legacy); err != nil {
		return fmt.Errorf("per-currency ledger: %w", err)
	}
	if legacy == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("per-currency ledger: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if engine == Postgres {
		// Every replica runs the chain at start: one re-homes, the others
		// wait and then find nothing left.
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('ae:rehome-ledger', 0))`); err != nil {
			return fmt.Errorf("per-currency ledger: lock: %w", err)
		}
	}
	if err := rehomeTx(ctx, tx, engine); err != nil {
		return fmt.Errorf("per-currency ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("per-currency ledger: commit: %w", err)
	}
	return nil
}

type legacyAccount struct {
	id         int64
	code, kind string
}

func rehomeTx(ctx context.Context, tx *sql.Tx, engine Engine) error {
	accounts, err := legacyAccounts(ctx, tx)
	if err != nil || len(accounts) == 0 {
		return err
	}
	a := newAttributor(ctx, tx)
	if err := a.load(); err != nil {
		return err
	}

	// Every journal with a line on a legacy account, attributed.
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT je.id, je.idem_key
		FROM journal_entries je
		JOIN ledger_entries le ON le.journal_id = je.id
		JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.currency IS NULL
		ORDER BY je.id`)
	if err != nil {
		return fmt.Errorf("journals: %w", err)
	}
	type journal struct {
		id  int64
		key string
	}
	var journals []journal
	for rows.Next() {
		var j journal
		if err := rows.Scan(&j.id, &j.key); err != nil {
			_ = rows.Close()
			return err
		}
		journals = append(journals, j)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	currencyOf := make(map[int64]string, len(journals))
	var unattributed []string
	for _, j := range journals {
		cur, err := a.journal(j.id, j.key)
		if err != nil {
			return err
		}
		if cur == "" {
			unattributed = append(unattributed, fmt.Sprintf("%d (%s)", j.id, j.key))
			continue
		}
		currencyOf[j.id] = cur
	}
	if len(unattributed) > 0 {
		shown := unattributed
		if len(shown) > 20 {
			shown = shown[:20]
		}
		return fmt.Errorf("cannot attribute %d journal(s) to a currency (the install has booked %s): %s; "+
			"nothing was changed. Each journal's currency must come from its commission, payout or raw event row",
			len(unattributed), strings.Join(a.currencies, ", "), strings.Join(shown, ", "))
	}

	// Lift the append-only guard for the move; restored before commit
	// (and guards_*.sql re-asserts it on every Apply regardless).
	if err := setEntriesUpdatable(ctx, tx, engine, true); err != nil {
		return err
	}
	var before int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries`).Scan(&before); err != nil {
		return err
	}
	moved := 0
	for _, acc := range accounts {
		n, err := rehomeAccount(ctx, tx, acc, currencyOf)
		if err != nil {
			return err
		}
		moved += n
	}
	if err := setEntriesUpdatable(ctx, tx, engine, false); err != nil {
		return err
	}

	// Verify before commit: nothing lost, nothing left on a legacy
	// account, every journal in one currency.
	var after, stranded, mixed int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries`).Scan(&after); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.currency IS NULL`).Scan(&stranded); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT le.journal_id FROM ledger_entries le
			JOIN ledger_accounts la ON la.id = le.account_id
			GROUP BY le.journal_id HAVING COUNT(DISTINCT la.currency) > 1) m`).Scan(&mixed); err != nil {
		return err
	}
	if after != before || stranded != 0 || mixed != 0 {
		return fmt.Errorf("verification failed: entries %d -> %d, %d left on legacy accounts, %d mixed journals",
			before, after, stranded, mixed)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ledger_accounts WHERE currency IS NULL
		  AND NOT EXISTS (SELECT 1 FROM ledger_entries le WHERE le.account_id = ledger_accounts.id)`); err != nil {
		return fmt.Errorf("remove legacy accounts: %w", err)
	}
	if len(a.currencies) == 1 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE program_settings SET currency = ? WHERE id = 1`, a.currencies[0]); err != nil {
			return fmt.Errorf("program currency: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (table_name, row_id, action, detail) VALUES ('ledger_accounts', 0, 'rehome', ?)`,
		fmt.Sprintf("per-currency ledger: moved %d lines of %d journals off %d legacy accounts",
			moved, len(journals), len(accounts))); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

func legacyAccounts(ctx context.Context, tx *sql.Tx) ([]legacyAccount, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, code, kind FROM ledger_accounts WHERE currency IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("legacy accounts: %w", err)
	}
	defer rows.Close()
	var out []legacyAccount
	for rows.Next() {
		var a legacyAccount
		if err := rows.Scan(&a.id, &a.code, &a.kind); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// rehomeAccount moves one legacy account's lines to its per-currency
// accounts (code + ":" + currency, the shape internal/ledger uses) and
// returns how many moved.
func rehomeAccount(ctx context.Context, tx *sql.Tx, acc legacyAccount, currencyOf map[int64]string) (int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT journal_id FROM ledger_entries WHERE account_id = ?`, acc.id)
	if err != nil {
		return 0, err
	}
	byCur := map[string][]int64{}
	for rows.Next() {
		var jid int64
		if err := rows.Scan(&jid); err != nil {
			_ = rows.Close()
			return 0, err
		}
		cur := currencyOf[jid]
		byCur[cur] = append(byCur[cur], jid)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	curs := make([]string, 0, len(byCur))
	for c := range byCur {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	moved := 0
	for _, cur := range curs {
		code := acc.code + ":" + cur
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_accounts (code, kind, currency) VALUES (?, ?, ?)
			ON CONFLICT(code) DO NOTHING`, code, acc.kind, cur); err != nil {
			return 0, fmt.Errorf("account %s: %w", code, err)
		}
		var target int64
		var tcur sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT id, currency FROM ledger_accounts WHERE code = ?`, code).Scan(&target, &tcur); err != nil {
			return 0, fmt.Errorf("account %s: %w", code, err)
		}
		if tcur.String != cur {
			return 0, fmt.Errorf("account %s exists with currency %q", code, tcur.String)
		}
		var res sql.Result
		if len(byCur) == 1 {
			res, err = tx.ExecContext(ctx,
				`UPDATE ledger_entries SET account_id = ? WHERE account_id = ?`, target, acc.id)
			if err != nil {
				return 0, fmt.Errorf("move %s: %w", acc.code, err)
			}
			n, _ := res.RowsAffected()
			moved += int(n)
			continue
		}
		jids := byCur[cur]
		for len(jids) > 0 {
			chunk := jids
			if len(chunk) > 500 {
				chunk = chunk[:500]
			}
			jids = jids[len(chunk):]
			args := []any{target, acc.id}
			for _, j := range chunk {
				args = append(args, j)
			}
			// #nosec G202 -- only "?" placeholders are concatenated.
			res, err = tx.ExecContext(ctx, `UPDATE ledger_entries SET account_id = ?
				WHERE account_id = ? AND journal_id IN (?`+strings.Repeat(", ?", len(chunk)-1)+`)`, args...)
			if err != nil {
				return 0, fmt.Errorf("move %s: %w", acc.code, err)
			}
			n, _ := res.RowsAffected()
			moved += int(n)
		}
	}
	return moved, nil
}

// setEntriesUpdatable lifts (on) or restores (off) the ledger_entries
// no-UPDATE guard inside the migration transaction. The SQLite definition
// matches guards_sqlite.sql; on Postgres the trigger is disabled and
// re-enabled, which is transactional.
func setEntriesUpdatable(ctx context.Context, tx *sql.Tx, engine Engine, on bool) error {
	var q string
	switch {
	case engine == Postgres && on:
		q = `ALTER TABLE ledger_entries DISABLE TRIGGER trg_ledger_entries_no_update`
	case engine == Postgres:
		q = `ALTER TABLE ledger_entries ENABLE TRIGGER trg_ledger_entries_no_update`
	case on:
		q = `DROP TRIGGER IF EXISTS trg_ledger_entries_no_update`
	default:
		q = `CREATE TRIGGER trg_ledger_entries_no_update BEFORE UPDATE ON ledger_entries
			BEGIN SELECT RAISE(ABORT, 'ledger_immutable'); END`
	}
	if _, err := tx.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("ledger guard: %w", err)
	}
	return nil
}

// attributor finds a historical journal's currency from the rows it was
// posted for.
type attributor struct {
	ctx context.Context
	tx  *sql.Tx

	byJournal  map[int64]string // commissions.journal_id -> currency
	byID       map[int64]string // commissions.id -> currency
	payouts    map[int64]string // payouts.id -> currency
	currencies []string         // every currency booked (commissions, payouts), sorted
}

func newAttributor(ctx context.Context, tx *sql.Tx) *attributor {
	return &attributor{
		ctx: ctx, tx: tx,
		byJournal: map[int64]string{}, byID: map[int64]string{}, payouts: map[int64]string{},
	}
}

func (a *attributor) load() error {
	seen := map[string]bool{}
	rows, err := a.tx.QueryContext(a.ctx, `SELECT id, journal_id, currency FROM commissions`)
	if err != nil {
		return fmt.Errorf("commissions: %w", err)
	}
	for rows.Next() {
		var id, jid int64
		var cur string
		if err := rows.Scan(&id, &jid, &cur); err != nil {
			_ = rows.Close()
			return err
		}
		cur = normCurrency(cur)
		a.byJournal[jid], a.byID[id] = cur, cur
		seen[cur] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = a.tx.QueryContext(a.ctx, `SELECT id, currency FROM payouts`)
	if err != nil {
		return fmt.Errorf("payouts: %w", err)
	}
	for rows.Next() {
		var id int64
		var cur string
		if err := rows.Scan(&id, &cur); err != nil {
			_ = rows.Close()
			return err
		}
		cur = normCurrency(cur)
		a.payouts[id] = cur
		seen[cur] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for c := range seen {
		a.currencies = append(a.currencies, c)
	}
	sort.Strings(a.currencies)
	return nil
}

var attemptSuffix = regexp.MustCompile(`#[0-9]+$`)

// journal returns the journal's currency, or "" when nothing attributes it.
func (a *attributor) journal(id int64, key string) (string, error) {
	if cur, ok := a.byJournal[id]; ok {
		return cur, nil
	}
	prefix, rest, _ := strings.Cut(key, ":")
	switch prefix {
	case "mature", "mature-revert", "fraudclaw", "fraudclaw-revert":
		if n, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if cur, ok := a.byID[n]; ok {
				return cur, nil
			}
		}
	case "payout":
		if n, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if cur, ok := a.payouts[n]; ok {
				return cur, nil
			}
		}
	case "commission", "purchase-revert":
		// The credit journal of the pair, then the event itself.
		var jid int64
		err := a.tx.QueryRowContext(a.ctx,
			`SELECT id FROM journal_entries WHERE idem_key = ?`, "commission:"+rest).Scan(&jid)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if cur, ok := a.byJournal[jid]; ok && err == nil {
			return cur, nil
		}
		return a.event(attemptSuffix.ReplaceAllString(rest, ""))
	case "clawback":
		return a.event(rest)
	case "usage":
		return "USD", nil
	}
	return a.fallback(), nil
}

// event reads the currency of the raw event "<source>:<external id>".
func (a *attributor) event(base string) (string, error) {
	source, ext, ok := strings.Cut(base, ":")
	if !ok {
		return a.fallback(), nil
	}
	var payload string
	err := a.tx.QueryRowContext(a.ctx,
		`SELECT CAST(payload AS TEXT) FROM raw_events WHERE source = ? AND external_event_id = ?`,
		source, ext).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return a.fallback(), nil
	}
	if err != nil {
		return "", fmt.Errorf("raw event %s: %w", base, err)
	}
	var ev struct{ Currency string }
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return a.fallback(), nil
	}
	return normCurrency(ev.Currency), nil
}

// fallback is the currency of a journal nothing else attributes: the
// install's only currency, or "" (unattributable) when it has several.
func (a *attributor) fallback() string {
	switch len(a.currencies) {
	case 0:
		return "USD"
	case 1:
		return a.currencies[0]
	}
	return ""
}

func normCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return "USD"
	}
	return c
}
