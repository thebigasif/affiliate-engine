package screening

// Backfill: screening for the commissions releases before screened_at
// credited and never screened. The replay repair (ProcessPurchase on
// ErrAlreadyProcessed) only reaches a commission when its provider
// delivers the event again, which for most sales is never. Backfill walks
// them all, with the same eligibility and the same ScreenFunc, each in its
// own ledger transaction.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// BackfillOptions shapes one Backfill pass.
type BackfillOptions struct {
	// DryRun screens each commission in its transaction and rolls it
	// back: the report says what would be held, nothing is written.
	DryRun bool
	// Limit caps the commissions examined in this pass (0 = all).
	Limit int
	// Since skips commissions created before it (zero = all).
	Since time.Time
	// Pace is a pause between commissions, so a pass on a live install
	// leaves the single SQLite writer to the webhooks (0 = none).
	Pace time.Duration
}

// BackfillReport is what a pass did (or, under DryRun, would do).
type BackfillReport struct {
	DryRun bool `json:"dry_run"`
	// Scanned: eligible commissions examined.
	Scanned int `json:"scanned"`
	// Screened: commissions screened (verdicts recorded, screened_at set).
	Screened int `json:"screened"`
	// Held: screened commissions that the screening held.
	Held int `json:"held"`
	// HeldBy counts the held commissions per rule (a hold naming two rules
	// counts under both; screening_error when a rule failed).
	HeldBy map[string]int `json:"held_by"`
	// Holds lists each held commission and its hold reason.
	Holds []BackfillHold `json:"holds"`
	// AlsoHeld: other commissions held through click-linked verdicts.
	AlsoHeld int `json:"also_held"`
	// Skipped: screened or held by something else after the scan
	// (a replayed delivery, a reviewer) and left alone.
	Skipped int `json:"skipped"`
	// Errors: commissions whose screening failed and rolled back. They
	// stay unscreened; the next pass retries them.
	Errors int `json:"errors"`
}

// BackfillHold is one commission a pass held (or would hold).
type BackfillHold struct {
	CommissionID int64  `json:"commission_id"`
	AffiliateID  int64  `json:"affiliate_id"`
	Reason       string `json:"reason"`
}

// backfillPage is how many candidates one scan reads.
const backfillPage = 100

// errDryRunRollback rolls a dry-run screening back after it is measured.
var errDryRunRollback = errors.New("screening: dry run (rolled back)")

// backfillCandidate is one unscreened commission and the event fields
// screening reads (the webhook's own event is not stored).
type backfillCandidate struct {
	id          int64
	affiliateID int64
	campaignID  int64
	email       string
	coupon      string
	idemKey     string
}

// event rebuilds what screening needs from the commission row. OccurredAt
// is left zero, so the rules evaluate as of now: their windows ("in the
// last hour", "in the last 7 days") have no upper bound, and evaluated at
// a months-old sale's time they would count every conversion since.
func (c backfillCandidate) event() commissions.Event {
	source, ext := "", ""
	key := strings.TrimPrefix(c.idemKey, "commission:")
	if i := strings.LastIndex(key, "#"); i >= 0 {
		key = key[:i]
	}
	if i := strings.Index(key, ":"); i >= 0 {
		source, ext = key[:i], key[i+1:]
	}
	return commissions.Event{
		Source: source, ExternalID: ext, EventType: "purchase",
		AffiliateID: c.affiliateID, CampaignID: c.campaignID,
		CustomerEmail: c.email, CouponCode: c.coupon,
	}
}

// Backfill screens every commission a sale credited that has never been
// screened (screened_at NULL), is pending or approved, and has no hold row
// (open or resolved): the replay repair's eligibility. Each commission is
// screened in its own ledger transaction through the same ScreenFunc as a
// fresh credit, so its verdicts and hold are what the credit would have
// written; on SQLite it goes through the ledger's single writer, on
// Postgres under the commission's row lock. A pass is idempotent and
// resumable: a screened commission is never picked again, and a failed
// one is left unscreened for the next pass. db is the read pool.
func (e *Engine) Backfill(ctx context.Context, db *sql.DB, opts BackfillOptions) (BackfillReport, error) {
	rep := BackfillReport{DryRun: opts.DryRun, HeldBy: map[string]int{}}
	if e.screener == nil {
		return rep, errors.New("screening: backfill needs a screener")
	}
	tp, ok := e.inner.(txPurchaser)
	if !ok {
		return rep, errors.New("screening: backfill needs the commissions engine")
	}
	since := "1970-01-01"
	if !opts.Since.IsZero() {
		// A whole day compares as a date: SQLite rows carry either
		// RFC 3339 or "YYYY-MM-DD HH:MM:SS", and both sort after it.
		since = opts.Since.UTC().Format(time.RFC3339)
		if s := opts.Since.UTC(); s.Equal(s.Truncate(24 * time.Hour)) {
			since = s.Format(time.DateOnly)
		}
	}
	var after int64
	for opts.Limit <= 0 || rep.Scanned < opts.Limit {
		page := backfillPage
		if opts.Limit > 0 && opts.Limit-rep.Scanned < page {
			page = opts.Limit - rep.Scanned
		}
		cands, err := backfillScan(ctx, db, after, since, page)
		if err != nil {
			return rep, err
		}
		if len(cands) == 0 {
			break
		}
		for _, c := range cands {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			after = c.id
			rep.Scanned++
			e.backfillOne(ctx, tp, c, opts.DryRun, &rep)
			if opts.Pace > 0 {
				select {
				case <-ctx.Done():
					return rep, ctx.Err()
				case <-time.After(opts.Pace):
				}
			}
		}
	}
	return rep, nil
}

func backfillScan(ctx context.Context, db *sql.DB, after int64, since string, limit int) ([]backfillCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.id, c.affiliate_id, c.campaign_id, c.buyer_email, c.coupon_code, j.idem_key
		FROM commissions c JOIN journal_entries j ON j.id = c.journal_id
		WHERE c.id > ? AND c.screened_at IS NULL
		  AND c.status IN ('pending','approved')
		  AND j.source = 'conversion'
		  AND c.created_at >= ?
		  AND NOT EXISTS (SELECT 1 FROM commission_holds h WHERE h.commission_id = c.id)
		ORDER BY c.id LIMIT ?`, after, since, limit)
	if err != nil {
		return nil, fmt.Errorf("screening: backfill scan: %w", err)
	}
	defer rows.Close()
	var out []backfillCandidate
	for rows.Next() {
		var c backfillCandidate
		if err := rows.Scan(&c.id, &c.affiliateID, &c.campaignID, &c.email, &c.coupon, &c.idemKey); err != nil {
			return nil, fmt.Errorf("screening: backfill scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("screening: backfill scan: %w", err)
	}
	return out, nil
}

// backfillOne screens one candidate and adds the outcome to rep.
func (e *Engine) backfillOne(ctx context.Context, tp txPurchaser, c backfillCandidate, dryRun bool, rep *BackfillReport) {
	screen := e.screenFunc(c.event())
	var skipped bool
	var reason string
	var others int
	err := tp.ScreenCommission(ctx, c.id, func(ctx context.Context, tx *ledger.Tx, cid int64) error {
		skipped, reason, others = false, "", 0 // the transaction may be retried
		open, held, err := screenState(ctx, tx, cid)
		if err != nil {
			return err
		}
		if !open || held {
			skipped = true
			return nil
		}
		before, err := holdCount(ctx, tx)
		if err != nil {
			return err
		}
		if err := screen(ctx, tx, cid); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx,
			`SELECT reason FROM commission_holds WHERE commission_id = ?`, cid).Scan(&reason)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("screening: commission %d hold: %w", cid, err)
		}
		total, err := holdCount(ctx, tx)
		if err != nil {
			return err
		}
		others = total - before
		if reason != "" {
			others--
		}
		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRunRollback) {
		rep.Errors++
		log.Printf("screening: backfill commission %d: %v", c.id, err)
		return
	}
	if skipped {
		rep.Skipped++
		return
	}
	rep.Screened++
	rep.AlsoHeld += others
	if reason == "" {
		return
	}
	rep.Held++
	rep.Holds = append(rep.Holds, BackfillHold{CommissionID: c.id, AffiliateID: c.affiliateID, Reason: reason})
	for _, rule := range strings.Split(strings.TrimPrefix(reason, "fraud review: "), ",") {
		rep.HeldBy[strings.TrimSpace(rule)]++
	}
}

func holdCount(ctx context.Context, tx *ledger.Tx) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM commission_holds`).Scan(&n); err != nil {
		return 0, fmt.Errorf("screening: count holds: %w", err)
	}
	return n, nil
}
