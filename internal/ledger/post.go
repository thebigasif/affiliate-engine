// Package ledger posts money movements to the double-entry journal.
// All methods run on the dedicated ledger connection inside a single
// transaction (BEGIN IMMEDIATE semantics on SQLite via the write conn).
//
// Run is the atomicity primitive of the money path: a journal post and
// the domain rows it pays for (commission, clawback, payout and outbox
// rows, the raw-event marker) commit in ONE transaction on the ledger
// connection, or none of them do. Post called with the ctx Run hands its
// fn joins that transaction.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
)

// Line is one posting line: an account and a signed minor-unit amount.
type Line struct {
	Account string // per-currency account code, e.g. "HOLD:USD", "AFFILIATE_PAYABLE:42:JPY"
	Amount  int64  // signed minor units; journal must sum to exactly 0
}

// Poster posts journals on the dedicated ledger connection.
type Poster struct {
	lc  *sql.Conn
	now func() time.Time
	pg  bool

	// mu serializes ALL use of lc. *sql.Conn is not safe for concurrent
	// use (database/sql docs): two goroutines driving Post/AccountBalance
	// on the one pinned session interleave their wire protocol and corrupt
	// it (observed on pgx as `sql: expected 0 destination arguments in
	// Scan` / `driver: bad connection` mid-post — the post then "fails"
	// while partially landing, and every CAS->post->revert caller treats
	// that as a post failure). The dedicated conn IS the serialization
	// point of the money path (SQLite single-writer; PG pinned session) —
	// this mutex makes that contract actually hold. It is shared by every
	// Poster bound to the same conn (connLock), and held for the whole of
	// a Run. Leaf lock: callers may hold commission moneylocks above it,
	// never the reverse.
	mu *sync.Mutex
}

// connLocks maps each ledger conn to its one mutex, so two Posters bound
// to the same conn still serialize (tests and CLI paths build several).
var connLocks sync.Map // *sql.Conn -> *sync.Mutex

func connLock(lc *sql.Conn) *sync.Mutex {
	m, _ := connLocks.LoadOrStore(lc, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// NewPoster binds a poster to the dedicated ledger connection.
func NewPoster(lc *sql.Conn) *Poster {
	return &Poster{lc: lc, now: time.Now, pg: db.IsPostgres(lc), mu: connLock(lc)}
}

// Post inserts a journal with its lines atomically. idemKey makes the call
// idempotent: an existing journal under the same key returns its id and
// ErrDuplicate (callers treat as success-no-op).
//
// Called with a ctx from inside Run, Post joins that transaction: the
// journal commits or rolls back with everything else fn writes.
//
// Invariants enforced here + by schema triggers:
//   - >= 2 lines
//   - sum of amounts == 0 exactly
//   - one currency: every line's account holds the same currency, so the
//     journal balances per currency (see accounts.go)
//   - account codes must exist, or be per-currency system codes (created
//     on first use); legacy currency-less accounts are refused
//   - no AFFILIATE_PAYABLE overdraft (checked in tx, per currency)
var ErrDuplicate = fmt.Errorf("ledger: duplicate idem key")

func (p *Poster) Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error) {
	return p.post(ctx, idemKey, source, description, effective, lines, false)
}

// PostClawback posts a refund clawback journal. It is Post with ONE
// difference: the AFFILIATE_PAYABLE overdraft guard is skipped, so a
// clawback may take an affiliate's payable balance POSITIVE.
//
// Why: a refund that lands after the commission was paid out must still
// claw the money back — the payable is already 0, so the clawback debit
// necessarily overshoots it. A positive payable is a receivable (the
// affiliate owes us); the payout runner nets it against future earnings
// before staging the next batch, and payouts keep going through Post, so
// the guard still stops any payout from paying more than is owed.
func (p *Poster) PostClawback(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error) {
	return p.post(ctx, idemKey, source, description, effective, lines, true)
}

// post is the shared implementation. allowPayableDebit disables the
// payable overdraft guard (clawbacks only — see PostClawback).
func (p *Poster) post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line, allowPayableDebit bool) (int64, error) {
	if err := checkLines(lines); err != nil {
		return 0, err
	}
	var id int64
	err := p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		var err error
		id, err = tx.post(ctx, idemKey, source, description, lines, allowPayableDebit)
		return err
	})
	if errors.Is(err, ErrDuplicate) {
		return id, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// checkLines enforces the line invariants before any tx is opened.
func checkLines(lines []Line) error {
	if len(lines) < 2 {
		return fmt.Errorf("ledger: need >=2 lines, got %d", len(lines))
	}
	var sum int64
	for _, l := range lines {
		sum += l.Amount
	}
	if sum != 0 {
		return fmt.Errorf("ledger: unbalanced journal: sum=%d", sum)
	}
	// Fast path of the one-currency rule for system codes; post re-checks
	// every line against the account's stored currency.
	var cur string
	for _, l := range lines {
		c, ok := AccountCurrency(l.Account)
		if !ok {
			continue
		}
		if cur != "" && c != cur {
			return fmt.Errorf("%w: %s and %s", ErrMixedCurrency, cur, c)
		}
		cur = c
	}
	return nil
}

// ErrMixedCurrency rejects a journal whose lines are in more than one
// currency. Money is never converted or netted across currencies.
var ErrMixedCurrency = errors.New("ledger: journal mixes currencies")

// errIdemRace marks a journal INSERT that lost a same-key race to a
// concurrent transaction (possible on PG's MVCC; SQLite's single writer
// serializes). PG aborts the whole transaction on the statement error, so
// the only recovery is a retry of the transaction, which then finds the
// winner's journal in the pre-check and reports ErrDuplicate. Run does it.
var errIdemRace = errors.New("ledger: idem key taken by a concurrent transaction")

// Tx is one Run transaction on the dedicated ledger connection: journal
// posts plus the domain DML that must commit with them. It must not
// escape fn (or the goroutine running it).
type Tx struct {
	tx  *sql.Tx
	lc  *sql.Conn
	now func() time.Time
	pg  bool
	// lockNS prefixes every advisory-lock key (Tx.LockKey). Empty for the
	// self-host pinned poster (historical keys unchanged); the shop schema
	// for PooledPoster, so the same entity in two shops is two locks
	// (PRD 12.4 item 2: keyed by shop, entity).
	lockNS string
	// pool is the PooledPoster owner identity (nil for the pinned
	// poster): a nested Run/Post/AccountBalance on a PooledPoster of the
	// same pool and schema joins this transaction (txFromPool).
	pool *sql.DB
}

// ExecContext runs domain DML inside the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryRowContext reads inside the transaction (sees its own writes).
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// QueryContext reads inside the transaction (sees its own writes).
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

// Postgres reports whether the transaction runs on Postgres.
func (t *Tx) Postgres() bool { return t.pg }

// ForUpdate is the row-lock suffix for a SELECT that reads the row a money
// transition decides on: " FOR UPDATE" on Postgres, where it makes every
// replica's transaction on that row queue behind this one (READ COMMITTED
// then re-reads the committed row); "" on SQLite, where the transaction
// already holds the database write lock from BEGIN IMMEDIATE.
func (t *Tx) ForUpdate() string {
	if t.pg {
		return " FOR UPDATE"
	}
	return ""
}

// LockKey takes a transaction-scoped Postgres advisory lock on key, for
// money state that has no single row to lock (an account balance). No-op
// on SQLite (the write lock is already held). PooledPoster namespaces the
// key with the shop schema, so the same entity in two shops never shares
// a lock.
func (t *Tx) LockKey(ctx context.Context, key string) error {
	if !t.pg {
		return nil
	}
	if _, err := t.tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, t.lockNS+key); err != nil {
		return fmt.Errorf("ledger: lock %q: %w", t.lockNS+key, err)
	}
	return nil
}

// Post inserts a journal inside the transaction. Same contract as
// Poster.Post (an existing journal under idemKey returns its id and
// ErrDuplicate); Run commits or rolls back everything together.
func (t *Tx) Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error) {
	if err := checkLines(lines); err != nil {
		return 0, err
	}
	return t.post(ctx, idemKey, source, description, lines, false)
}

// PostClawback is Post without the payable overdraft guard (see
// Poster.PostClawback).
func (t *Tx) PostClawback(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error) {
	if err := checkLines(lines); err != nil {
		return 0, err
	}
	return t.post(ctx, idemKey, source, description, lines, true)
}

// post inserts the journal + lines in the transaction. effective_at
// records the posting time, as it always has; the effective argument of
// the public Post signatures is not stored.
func (t *Tx) post(ctx context.Context, idemKey, source, description string, lines []Line, allowPayableDebit bool) (int64, error) {
	// Idempotency: existing journal under this key?
	var existing int64
	err := t.tx.QueryRowContext(ctx,
		`SELECT id FROM journal_entries WHERE idem_key = ?`, idemKey).Scan(&existing)
	switch {
	case err == nil:
		return existing, ErrDuplicate
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("ledger: idem lookup: %w", err)
	}

	// Payable overdraft guard, cross-replica: the balance read below must
	// see every committed payout of this affiliate, so on Postgres the
	// guarded accounts are locked first (sorted: no lock-order deadlock).
	if !allowPayableDebit {
		var guarded []string
		for _, l := range lines {
			if l.Amount > 0 && isAffiliatePayable(l.Account) {
				guarded = append(guarded, l.Account)
			}
		}
		slices.Sort(guarded)
		for _, code := range slices.Compact(guarded) {
			if err := t.LockKey(ctx, "ledger:"+code); err != nil {
				return 0, err
			}
		}
	}

	// Resolve every account (creating per-currency system accounts on
	// first use) and hold the journal to one currency.
	accountIDs := make([]int64, len(lines))
	var currency string
	for i, l := range lines {
		id, cur, err := t.account(ctx, l.Account)
		if err != nil {
			return 0, err
		}
		if currency != "" && cur != currency {
			return 0, fmt.Errorf("%w: %q is %s, journal is %s", ErrMixedCurrency, l.Account, cur, currency)
		}
		accountIDs[i], currency = id, cur
	}

	// RETURNING works on both engines (SQLite >= 3.35, PG) and avoids
	// LastInsertId, which pgx does not support.
	var journalID int64
	if err := t.tx.QueryRowContext(ctx,
		`INSERT INTO journal_entries (idem_key, source, description, effective_at, created_at)
		 VALUES (?, ?, ?, ?, ?) RETURNING id`,
		idemKey, source, description, t.now().UTC(), t.now().UTC()).Scan(&journalID); err != nil {
		if db.IsUniqueViolation(err) {
			return 0, fmt.Errorf("%w: %q: %w", errIdemRace, idemKey, err)
		}
		return 0, fmt.Errorf("ledger: insert journal: %w", err)
	}

	for i, l := range lines {
		accountID := accountIDs[i]
		// Overdraft guard for payable accounts (before posting the line).
		// Convention: payable is a LIABILITY tracked negative (credit = we owe
		// more). Overdraft = a payout (positive line) that would push the
		// balance above zero — paying out more than we owe. Clawbacks are
		// exempt (PostClawback): a post-payout refund legitimately turns
		// the payable into a receivable netted against future earnings.
		if !allowPayableDebit && l.Amount > 0 && isAffiliatePayable(l.Account) {
			var bal int64
			if err := t.tx.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(amount), 0) FROM ledger_entries
				WHERE account_id = ?`, accountID).Scan(&bal); err != nil {
				return 0, fmt.Errorf("ledger: balance check %q: %w", l.Account, err)
			}
			if bal+l.Amount > 0 {
				return 0, fmt.Errorf("ledger: overdraft %q: bal=%d line=%d", l.Account, bal, l.Amount)
			}
		}
		if _, err := t.tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (journal_id, account_id, amount, created_at)
			 VALUES (?, ?, ?, ?)`, journalID, accountID, l.Amount, t.now().UTC()); err != nil {
			return 0, fmt.Errorf("ledger: insert line: %w", err)
		}
	}
	return journalID, nil
}

// account resolves an account code to its id and currency inside the
// transaction. A missing per-currency system account is created (an
// affiliate payable only for an existing affiliate). A legacy account
// without a currency is refused: the per-currency migration re-homes its
// balance, and nothing may post to it again.
func (t *Tx) account(ctx context.Context, code string) (int64, string, error) {
	const q = `SELECT id, currency FROM ledger_accounts WHERE code = ?`
	var id int64
	var cur sql.NullString
	err := t.tx.QueryRowContext(ctx, q, code).Scan(&id, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		spec, ok := parseAccount(code)
		if !ok {
			return 0, "", fmt.Errorf("ledger: account %q: %w", code, err)
		}
		if spec.affiliateID > 0 {
			var n int
			if err := t.tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM affiliates WHERE id = ?`, spec.affiliateID).Scan(&n); err != nil {
				return 0, "", fmt.Errorf("ledger: account %q: %w", code, err)
			}
			if n == 0 {
				return 0, "", fmt.Errorf("ledger: account %q: no affiliate %d", code, spec.affiliateID)
			}
		}
		if _, err := t.tx.ExecContext(ctx, `
			INSERT INTO ledger_accounts (code, kind, currency) VALUES (?, ?, ?)
			ON CONFLICT(code) DO NOTHING`, code, spec.kind, spec.currency); err != nil {
			return 0, "", fmt.Errorf("ledger: create account %q: %w", code, err)
		}
		err = t.tx.QueryRowContext(ctx, q, code).Scan(&id, &cur)
	}
	if err != nil {
		return 0, "", fmt.Errorf("ledger: account %q: %w", code, err)
	}
	if !cur.Valid || cur.String == "" {
		return 0, "", fmt.Errorf("ledger: account %q has no currency (a legacy account: run the migrations)", code)
	}
	return id, cur.String, nil
}

// txKey carries the open Run transaction in its fn's ctx.
type txKey struct{}

// txFrom returns the Run transaction ctx carries on conn lc, if any.
func txFrom(ctx context.Context, lc *sql.Conn) *Tx {
	t, _ := ctx.Value(txKey{}).(*Tx)
	if t == nil || t.lc == nil || t.lc != lc {
		return nil
	}
	return t
}

// txFromPool returns the PooledPoster Run transaction ctx carries for
// pool and the shop's lock namespace, if any.
func txFromPool(ctx context.Context, pool *sql.DB, lockNS string) *Tx {
	t, _ := ctx.Value(txKey{}).(*Tx)
	if t == nil || t.pool == nil || t.pool != pool || t.lockNS != lockNS {
		return nil
	}
	return t
}

// WithTx returns ctx carrying tx, so a Poster or PooledPoster call made
// with it joins tx (the same thing Run does for its fn).
func WithTx(ctx context.Context, tx *Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// runAttempts bounds Run's retries of a transaction that lost a race it
// can only recover from by starting over (errIdemRace, PG deadlock or
// serialization failure). Each retry re-reads everything under its locks.
const runAttempts = 3

// Run executes fn inside ONE transaction on the dedicated ledger
// connection. Everything fn does — journal posts (Tx.Post, or Post with
// the ctx fn is given) and domain DML through the Tx — commits atomically,
// or none of it does. fn returning an error rolls everything back.
//
// This replaces the CAS->post->revert shape, where the journal committed
// on the ledger conn and the domain rows on the pool: a failure between
// them left a journal with no commission (or a status with no journal),
// and the "revert" was a second post whose error was ignored.
//
// fn may be run more than once: a transaction that loses a race it can
// only recover from by starting over (a concurrent same-key journal, a PG
// deadlock or serialization failure) is retried. fn must therefore derive
// everything from what it reads inside the transaction, and set any
// results it hands out on every run.
//
// Nested use joins: Run (or Post, AccountBalance) called with the ctx of
// an open Run on the same conn runs inside that transaction instead of
// waiting for the mutex it holds. The ctx must not leave fn's goroutine.
func (p *Poster) Run(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error {
	if t := txFrom(ctx, p.lc); t != nil {
		return fn(ctx, t)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var err error
	for attempt := 0; attempt < runAttempts; attempt++ {
		err = p.runOnce(ctx, fn)
		if err == nil || !(errors.Is(err, errIdemRace) || db.IsTxConflict(err)) {
			return err
		}
	}
	return err
}

func (p *Poster) runOnce(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error {
	stx, err := p.lc.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin: %w", err)
	}
	defer stx.Rollback()
	t := &Tx{tx: stx, lc: p.lc, now: p.now, pg: p.pg}
	if err := fn(context.WithValue(ctx, txKey{}, t), t); err != nil {
		return err
	}
	if err := stx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit: %w", err)
	}
	return nil
}

// AccountBalance returns the derived balance (SUM of entries) for an account.
func (p *Poster) AccountBalance(ctx context.Context, code string) (int64, error) {
	var bal int64
	if err := p.balance(ctx, code, &bal); err != nil {
		return 0, fmt.Errorf("ledger: balance %q: %w", code, err)
	}
	return bal, nil
}

func (p *Poster) balance(ctx context.Context, code string, bal *int64) error {
	if t := txFrom(ctx, p.lc); t != nil {
		return t.tx.QueryRowContext(ctx, balanceSQL, code).Scan(bal)
	}
	// The Scan must run under the mutex too: the row holds the conn.
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lc.QueryRowContext(ctx, balanceSQL, code).Scan(bal)
}

const balanceSQL = `
	SELECT COALESCE(SUM(le.amount), 0)
	FROM ledger_entries le
	JOIN ledger_accounts la ON la.id = le.account_id
	WHERE la.code = ?`

// GlobalIntegrity returns the sum of ALL ledger entries. Must always be 0.
func (p *Poster) GlobalIntegrity(ctx context.Context) (int64, error) {
	var sum int64
	q := `SELECT COALESCE(SUM(amount), 0) FROM ledger_entries`
	var err error
	if t := txFrom(ctx, p.lc); t != nil {
		err = t.tx.QueryRowContext(ctx, q).Scan(&sum)
	} else {
		p.mu.Lock()
		err = p.lc.QueryRowContext(ctx, q).Scan(&sum)
		p.mu.Unlock()
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: integrity: %w", err)
	}
	return sum, nil
}

// EnsureAccounts creates ledger accounts idempotently. Every code must be
// a per-currency account code (accounts.go), whose currency is stored
// with it. Post creates system accounts on first use, so this only
// pre-creates them.
func (p *Poster) EnsureAccounts(ctx context.Context, codes map[string]string) error {
	return p.Run(ctx, func(ctx context.Context, tx *Tx) error {
		for code, kind := range codes {
			cur, ok := AccountCurrency(code)
			if !ok {
				return fmt.Errorf("ledger: ensure account %q: not a per-currency account code", code)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ledger_accounts (code, kind, currency) VALUES (?, ?, ?)
				ON CONFLICT(code) DO NOTHING`, code, kind, cur); err != nil {
				return fmt.Errorf("ledger: ensure account %q: %w", code, err)
			}
		}
		return nil
	})
}
