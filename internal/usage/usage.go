// Package usage implements metered/usage-based commission attribution —
// commissions that track what the customer actually consumed (tokens,
// credits, API calls), not just what they paid. This is the AI-SaaS wedge:
// no incumbent affiliate tracker meters usage (verified: FirstPromoter's
// AI-companies page has zero usage/metered/token features).
//
// Design: usage events land idempotently in usage_events, roll up into
// usage_daily meters, and a settlement job converts meter deltas into
// commissions at per-unit rates via the normal money path (ledger posting,
// exactly-once keys). Token-scale volumes never create per-event commission
// rows — the meter delta is the commission basis.
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/bpmath"
	"github.com/thebigasif/affiliate-engine/internal/dbutil"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// Ingestor absorbs usage events.
type Ingestor struct {
	db *sql.DB
}

// New builds one.
func New(db *sql.DB) *Ingestor { return &Ingestor{db: db} }

// Event is one metered consumption record.
type Event struct {
	Source        string // "api", "stripe_usage", "mcp", ...
	ExternalID    string // provider event id (idempotency)
	CustomerExtID string
	Units         int64  // tokens/credits/seconds
	UnitType      string // "tokens" | "credits" | "calls" | "seconds"
	OccurredAt    time.Time
}

var ErrDuplicate = fmt.Errorf("usage: duplicate event")

// Ingest stores one event idempotently and rolls the daily meter.
func (in *Ingestor) Ingest(ctx context.Context, ev Event) error {
	if ev.Units < 0 {
		return fmt.Errorf("usage: negative units")
	}
	if ev.CustomerExtID == "" || ev.ExternalID == "" {
		return fmt.Errorf("usage: customer and external id required")
	}
	if ev.UnitType == "" {
		ev.UnitType = "tokens"
	}
	tx, err := in.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events (source, external_id, customer_ext_id, units, unit_type, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		ev.Source, ev.ExternalID, ev.CustomerExtID, ev.Units, ev.UnitType,
		ev.OccurredAt.UTC().Format(time.RFC3339Nano)); err != nil {
		if isUniqueErr(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("usage: insert: %w", err)
	}
	day := ev.OccurredAt.UTC().Format("20060102")
	var dayInt int64
	for _, c := range day {
		dayInt = dayInt*10 + int64(c-'0')
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_daily (day, customer_ext_id, unit_type, units)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(day, customer_ext_id, unit_type)
		DO UPDATE SET units = usage_daily.units + ?`,
		dayInt, ev.CustomerExtID, ev.UnitType, ev.Units, ev.Units); err != nil {
		return fmt.Errorf("usage: meter: %w", err)
	}
	return tx.Commit()
}

// Settler converts meter growth into commissions.
type Settler struct {
	db     *sql.DB
	Poster Poster
	// Screen screens each usage commission inside the settlement
	// transaction, right after its row is written, like a purchase is
	// screened in the transaction that credits it: its verdicts and hold
	// commit with the commission or not at all. An error rolls the whole
	// settlement back. nil = not screened.
	Screen func(ctx context.Context, tx Tx, commissionID int64) error
}

// Poster is the ledger slice (satisfied by *ledger.Poster through a thin
// adapter: the shapes are identical, the types distinct to keep this
// package dependency-light).
//
// Run executes fn in ONE ledger transaction: Post called with the ctx Run
// hands fn joins it, and fn's DML goes through the Tx. SettleDay commits
// each day's journals, commission rows and settled markers together.
type Poster interface {
	Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []Line) (int64, error)
	Run(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx is the transaction surface Run hands fn (*ledger.Tx satisfies it).
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ForUpdate() string
}

// Line mirrors ledger.Line (account + signed minor units).
type Line struct {
	Account string
	Amount  int64
}

// NewSettler builds one.
func NewSettler(db *sql.DB, p Poster) *Settler { return &Settler{db: db, Poster: p} }

// SettleDay commissions the unsettled meter delta for one day: every
// customer whose usage attributed to an affiliate earns
// delta_units x unit_rate_bp (per 10k units, minor units).
//
// ATOMIC: the day's journals, commission rows and settled_cents markers
// commit in ONE ledger transaction, each meter row read under lock (FOR
// UPDATE on Postgres) so two replicas settling the same day cannot both
// pay its delta. A failure settles nothing; the re-run starts clean.
func (s *Settler) SettleDay(ctx context.Context, day int64, unitRateBP int64) (commissions int64, err error) {
	// A negative rate would mint negative bases; reject it before touching
	// any meter.
	if unitRateBP < 0 {
		return 0, fmt.Errorf("usage: negative unit rate %d bp", unitRateBP)
	}
	months := settings.CommissionMonths(ctx, s.db, 1)
	var dayStart time.Time
	if months > 0 {
		if dayStart, err = time.Parse("20060102", fmt.Sprintf("%08d", day)); err != nil {
			return 0, fmt.Errorf("usage: day %d is not YYYYMMDD: %w", day, err)
		}
	}
	// NET terms from the program, mirroring ProcessPurchase. This was
	// NULL once: Mature() selects `matures_at <= now`, and NULL never
	// compares true on either engine — every usage commission was stuck
	// 'pending' forever with its money stranded in HOLD (FR-16 broken).
	netDays := settings.HoldDays(ctx, s.db, 1)

	err = s.Poster.Run(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		commissions, err = s.settleTx(ctx, tx, day, unitRateBP, months, dayStart, netDays)
		return err
	})
	if err != nil {
		return 0, err
	}
	return commissions, nil
}

type meter struct {
	customer, unitType string
	units, settled     int64
	affiliateID        int64
	boundAt            any
}

func (s *Settler) settleTx(ctx context.Context, tx Tx, day, unitRateBP int64, months int, dayStart time.Time, netDays int) (int64, error) {
	// The day's attributed meters, through the pool: the key set only.
	// Each one's units and settled baseline are re-read under lock below.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ud.customer_ext_id, ud.unit_type, c.affiliate_id, c.bound_at
		FROM usage_daily ud
		JOIN customers c ON c.external_customer_id = ud.customer_ext_id AND c.program_id = 1
		JOIN affiliates a ON a.id = c.affiliate_id AND a.status = 'active'
		WHERE ud.day = ?
		ORDER BY ud.customer_ext_id, ud.unit_type`, day)
	if err != nil {
		return 0, err
	}
	var meters []meter
	for rows.Next() {
		var m meter
		if err := rows.Scan(&m.customer, &m.unitType, &m.affiliateID, &m.boundAt); err != nil {
			_ = rows.Close()
			return 0, err
		}
		meters = append(meters, m)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i := range meters {
		m := &meters[i]
		if err := tx.QueryRowContext(ctx, `
			SELECT units, settled_cents FROM usage_daily
			WHERE day = ? AND customer_ext_id = ? AND unit_type = ?`+tx.ForUpdate(),
			day, m.customer, m.unitType).Scan(&m.units, &m.settled); err != nil {
			return 0, fmt.Errorf("usage: meter %s/%s: %w", m.customer, m.unitType, err)
		}
	}

	// Checked math via bpmath (128-bit exact): token-scale volumes (1e18
	// units) at percentage rates must never wrap negative, nor saturate
	// into a plausible-looking MaxInt64 basis that pays real money. Every
	// basis is validated BEFORE any journal posts, so an unrepresentable
	// meter rejects the run.
	bases := make([]int64, len(meters))
	for i, m := range meters {
		b, ok := bpmath.MulDivBP(m.units, unitRateBP)
		if !ok {
			return 0, fmt.Errorf("usage: settlement basis overflow: customer=%s units=%d rate_bp=%d",
				m.customer, m.units, unitRateBP)
		}
		bases[i] = b
	}

	matures := time.Now().UTC().Add(time.Duration(netDays) * 24 * time.Hour).Format(time.RFC3339Nano)
	var commissions int64
	for i, m := range meters {
		basis := bases[i]
		delta := basis - m.settled
		if delta <= 0 {
			continue
		}
		// Commission period (campaigns.commission_months, 0 = lifetime):
		// usage on days after it ends earns nothing, like purchases.
		if start, ok := dbutil.AsTime(m.boundAt); ok && months > 0 && !dayStart.Before(start.AddDate(0, months, 0)) {
			continue
		}
		// The key names the settled baseline the delta starts FROM, not
		// the basis it reaches: settled_cents only advances with the
		// commission row, so a retried settle reuses the same key even
		// after more usage raised the basis.
		idem := fmt.Sprintf("usage:%d:%s:%s:from:%d", day, m.customer, m.unitType, m.settled)
		// Usage commissions are booked in USD (the unit rate is in USD
		// minor units), on the USD accounts.
		journalID, err := s.Poster.Post(ctx, idem, "usage_settlement", m.customer,
			time.Unix(0, 0).UTC(), []Line{
				{Account: "EXPENSE:COMMISSION:USD", Amount: delta},
				{Account: "HOLD:USD", Amount: -delta},
			})
		if err != nil {
			// F4 recovery: a duplicate idem key with no commission row is
			// a journal from a release that posted outside the settle's
			// transaction and failed its domain writes. Adopt it: it
			// settles exactly what it posted (usage may have grown since);
			// the next run settles the rest under the next key.
			if !strings.Contains(err.Error(), "duplicate idem") {
				return 0, fmt.Errorf("usage: ledger post: %w", err)
			}
			var jid, amount int64
			if qerr := tx.QueryRowContext(ctx, `
				SELECT je.id, COALESCE(SUM(le.amount), 0)
				FROM journal_entries je
				JOIN ledger_entries le ON le.journal_id = je.id
				JOIN ledger_accounts la ON la.id = le.account_id
				WHERE je.idem_key = ? AND la.code = 'EXPENSE:COMMISSION:USD'
				GROUP BY je.id`, idem).Scan(&jid, &amount); qerr != nil || amount <= 0 {
				return 0, fmt.Errorf("usage: ledger post: %w", err)
			}
			var owned int
			if qerr := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM commissions WHERE journal_id = ?`, jid).Scan(&owned); qerr != nil {
				return 0, fmt.Errorf("usage: adopt lookup: %w", qerr)
			}
			if owned > 0 {
				return 0, fmt.Errorf("usage: journal %s already settled but %s/%s settled_cents=%d: operator attention required",
					idem, m.customer, m.unitType, m.settled)
			}
			journalID, delta, basis = jid, amount, m.settled+amount
		}
		var commissionID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency, rate_snapshot, status, matures_at, created_at)
			VALUES (?, 1, ?, ?, 'USD', ?, 'pending', ?, ?)
			RETURNING id`,
			m.affiliateID, journalID, delta,
			fmt.Sprintf(`{"origin":"usage","unit_type":%q,"units":%d,"rate_bp":%d}`, m.unitType, m.units, unitRateBP),
			matures, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&commissionID); err != nil {
			return 0, fmt.Errorf("usage: commission row: %w", err)
		}
		if s.Screen != nil {
			if err := s.Screen(ctx, tx, commissionID); err != nil {
				return 0, fmt.Errorf("usage: screen commission %d: %w", commissionID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_daily SET settled_cents = ? WHERE day = ? AND customer_ext_id = ? AND unit_type = ?`,
			basis, day, m.customer, m.unitType); err != nil {
			return 0, fmt.Errorf("usage: settled marker: %w", err)
		}
		commissions++
	}
	return commissions, nil
}

func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "UNIQUE constraint failed") || contains(s, "23505")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
