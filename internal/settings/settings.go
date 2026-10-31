// Package settings reads and writes the program-wide payout settings
// (program_settings, a singleton row): the minimum payout a run pays and
// the default hold (NET terms) for new programs and maturity fallback.
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Defaults used when the row is missing (a database migrated before the
// settings table existed, or a hand-deleted row).
const (
	DefaultMinPayoutCents int64 = 5000 // $50
	DefaultHoldDays       int   = 30
	MaxHoldDays           int   = 365
)

// Program is the program-wide payout configuration.
//
// Currency is the program currency: a commission rule, coupon or volume
// tier without its own currency is in it, and MinPayoutCents is in its
// minor units. MinPayoutByCurrency sets the minimum payout of other
// currencies (minor units of each); a currency without an entry uses
// MinPayoutCents as it is (the behaviour before per-currency thresholds:
// 5000 is $50.00, but also ¥5,000).
type Program struct {
	MinPayoutCents      int64            `json:"min_payout_cents"`
	DefaultHoldDays     int              `json:"default_hold_days"`
	Currency            string           `json:"currency"`
	MinPayoutByCurrency map[string]int64 `json:"min_payout_by_currency"`
}

// Defaults returns the built-in settings.
func Defaults() Program {
	return Program{
		MinPayoutCents: DefaultMinPayoutCents, DefaultHoldDays: DefaultHoldDays,
		Currency: money.Default, MinPayoutByCurrency: map[string]int64{},
	}
}

// MinPayout is the minimum payout in one currency, in its minor units.
func (p Program) MinPayout(currency string) int64 {
	if v, ok := p.MinPayoutByCurrency[money.Normalize(currency)]; ok {
		return v
	}
	return p.MinPayoutCents
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Load reads the settings row. A missing row yields the defaults; any
// other read error is returned with the defaults (callers on a money path
// may choose to proceed on defaults, never on zero values). The
// per-currency minimums are read when q can run a multi-row query.
func Load(ctx context.Context, q rowQuerier) (Program, error) {
	p := Defaults()
	err := q.QueryRowContext(ctx,
		`SELECT min_payout_cents, default_hold_days, currency FROM program_settings WHERE id = 1`).
		Scan(&p.MinPayoutCents, &p.DefaultHoldDays, &p.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), fmt.Errorf("settings: load: %w", err)
	}
	p.Currency = money.Normalize(p.Currency)
	if rq, ok := q.(rowsQuerier); ok {
		rows, err := rq.QueryContext(ctx, `SELECT currency, min_minor FROM payout_thresholds`)
		if err != nil {
			return Defaults(), fmt.Errorf("settings: load thresholds: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			var v int64
			if err := rows.Scan(&c, &v); err != nil {
				return Defaults(), fmt.Errorf("settings: load thresholds: %w", err)
			}
			p.MinPayoutByCurrency[money.Normalize(c)] = v
		}
		if err := rows.Err(); err != nil {
			return Defaults(), fmt.Errorf("settings: load thresholds: %w", err)
		}
	}
	return p, nil
}

// HoldDays is the hold a commission of campaignID gets: the program's own
// net_terms_days when its row is readable (0 = matures immediately), else
// the program-wide default_hold_days, else DefaultHoldDays.
func HoldDays(ctx context.Context, q rowQuerier, campaignID int64) int {
	var nd int
	if err := q.QueryRowContext(ctx,
		`SELECT net_terms_days FROM campaigns WHERE id = ?`, campaignID).Scan(&nd); err == nil &&
		nd >= 0 && nd <= MaxHoldDays {
		return nd
	}
	p, _ := Load(ctx, q)
	return p.DefaultHoldDays
}

// HoldDaysAtLeast is HoldDays raised to floor: a deployment with a hold
// floor (the Shopify app's FR-33 refund-window floor) never matures a
// commission sooner, whatever the program row says. floor <= 0 = none.
func HoldDaysAtLeast(ctx context.Context, q rowQuerier, campaignID int64, floor int) int {
	return max(HoldDays(ctx, q, campaignID), floor)
}

// Currency is the program currency (program_settings.currency): what a
// commission rule, coupon or volume tier without its own currency is
// denominated in, and the currency min_payout_cents is set in. USD when
// the row is unreadable.
func Currency(ctx context.Context, q rowQuerier) string {
	var c string
	if err := q.QueryRowContext(ctx,
		`SELECT currency FROM program_settings WHERE id = 1`).Scan(&c); err != nil {
		return money.Default
	}
	return money.Normalize(c)
}

// MaxCommissionMonths bounds campaigns.commission_months (0 = lifetime).
const MaxCommissionMonths = 120

// CommissionMonths is how many months a customer keeps earning its
// affiliate commission in the program (campaigns.commission_months;
// 0 = for the customer's lifetime, also when the program row is missing).
func CommissionMonths(ctx context.Context, q rowQuerier, campaignID int64) int {
	var m int
	if err := q.QueryRowContext(ctx,
		`SELECT commission_months FROM campaigns WHERE id = ?`, campaignID).Scan(&m); err != nil ||
		m < 0 || m > MaxCommissionMonths {
		return 0
	}
	return m
}

// Normalize upper-cases the currency codes (an empty program currency is
// USD).
func (p *Program) Normalize() {
	p.Currency = money.Normalize(p.Currency)
	byCur := make(map[string]int64, len(p.MinPayoutByCurrency))
	for c, v := range p.MinPayoutByCurrency {
		byCur[money.Normalize(c)] = v
	}
	p.MinPayoutByCurrency = byCur
}

// Validate checks bounds.
func (p Program) Validate() error {
	if p.MinPayoutCents < 0 {
		return errors.New("min_payout_cents must be >= 0")
	}
	if p.DefaultHoldDays < 0 || p.DefaultHoldDays > MaxHoldDays {
		return fmt.Errorf("default_hold_days must be 0-%d", MaxHoldDays)
	}
	if !money.Valid(p.Currency) {
		return errors.New("currency must be a 3-letter ISO 4217 code")
	}
	for c, v := range p.MinPayoutByCurrency {
		if !money.Valid(c) {
			return fmt.Errorf("min_payout_by_currency: %q is not a 3-letter ISO 4217 code", c)
		}
		if v < 0 {
			return fmt.Errorf("min_payout_by_currency: %s must be >= 0", c)
		}
	}
	return nil
}

// Save upserts the settings row and replaces the per-currency minimums,
// in one transaction.
func Save(ctx context.Context, db *sql.DB, p Program) error {
	p.Normalize()
	if err := p.Validate(); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: save: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO program_settings (id, min_payout_cents, default_hold_days, currency, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			min_payout_cents = excluded.min_payout_cents,
			default_hold_days = excluded.default_hold_days,
			currency = excluded.currency,
			updated_at = excluded.updated_at`,
		p.MinPayoutCents, p.DefaultHoldDays, p.Currency, now); err != nil {
		return fmt.Errorf("settings: save: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM payout_thresholds`); err != nil {
		return fmt.Errorf("settings: save thresholds: %w", err)
	}
	for c, v := range p.MinPayoutByCurrency {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO payout_thresholds (currency, min_minor, updated_at) VALUES (?, ?, ?)`, c, v, now); err != nil {
			return fmt.Errorf("settings: save thresholds: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: save: %w", err)
	}
	return nil
}
