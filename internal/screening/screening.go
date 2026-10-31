// Package screening wraps the commissions engine with fraud evaluation at
// the conversion boundary: every purchase from a billing webhook is
// screened as it is credited, so verdicts bind to money at the earliest
// possible moment (holds land pre-maturation, not post-payout).
//
// Ordering: the commission is credited FIRST and then evaluated with its
// id in the query. Screening before the insert (the old order) meant a
// verdict could never hold the very commission that triggered it — a
// self-referral credited and matured untouched.
//
// Atomicity: the evaluation runs inside the credit's own ledger
// transaction (commissions.ScreenFunc, fraud.WithTx). The verdicts, the
// hold and the commission's screened_at commit with the journal and the
// commission row, or the credit rolls back. Screening after the commit
// (the previous release) left a flagged commission unheld whenever the
// process died or the hold write failed in between; a replayed delivery
// repairs such a commission.
//
// This closes the audit gap "fraud engine has zero production callers": the
// ruleset now runs on every conversion and on a periodic sweep, populating
// the fraud queue a merchant's admin console already knows how to display.
package screening

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// Purchaser is the commissions-engine slice the decorator forwards to
// (mirrors billing.ProcessPurchaser — kept local to avoid a package cycle).
type Purchaser interface {
	ProcessPurchase(ctx context.Context, ev commissions.Event) (int64, error)
	ProcessRefund(ctx context.Context, ev commissions.Event, originalCommissionID, refundedMinor, originalMinor int64) error
}

// cumulativeRefunder is the optional inner capability behind
// ProcessRefundCumulative (*commissions.Engine implements it).
type cumulativeRefunder interface {
	ProcessRefundCumulative(ctx context.Context, ev commissions.Event, originalCommissionID, cumulativeRefundedMinor, originalMinor int64) error
}

var _ cumulativeRefunder = (*commissions.Engine)(nil)

// customerBinder is the optional inner capability behind BindCustomer.
type customerBinder interface {
	BindCustomer(ctx context.Context, customerExtID string, affiliateID, campaignID int64, method string) error
}

var _ customerBinder = (*commissions.Engine)(nil)

// creditedFinder is the optional inner capability behind CreditedCommission.
type creditedFinder interface {
	CreditedCommission(ctx context.Context, source, externalID string) (int64, error)
}

var _ creditedFinder = (*commissions.Engine)(nil)

// Screener is the fraud-engine slice the decorator calls after crediting.
type Screener interface {
	Evaluate(ctx context.Context, q fraud.Query) ([]fraud.Verdict, error)
}

// CommissionLookup resolves a credited commission's evaluation context
// (affiliate, program, originating click ip hash). *fraud.Engine
// implements it; screeners without it are queried with event data only.
type CommissionLookup interface {
	LookupCommission(ctx context.Context, commissionID int64) (fraud.CommissionContext, error)
}

// Holder places a payment hold on one commission. *fraud.Engine
// implements it; it is what makes screening fail closed.
type Holder interface {
	HoldCommission(ctx context.Context, commissionID int64, reason string) error
}

// ScreeningErrorReason is the hold reason used when screening itself
// fails: the commission is held for review instead of credited silently.
const ScreeningErrorReason = "screening_error"

// Engine decorates a Purchaser with fraud screening.
type Engine struct {
	inner    Purchaser
	screener Screener
	// dryRun screens but never enforces (shadow mode rollout).
	dryRun bool
}

// New wraps inner. screener may be nil (no-op pass-through).
func New(inner Purchaser, screener Screener, dryRun bool) *Engine {
	return &Engine{inner: inner, screener: screener, dryRun: dryRun}
}

// ProcessPurchase credits and screens the new commission in ONE
// transaction: the verdict and the hold it implies commit with the
// journal and the commission row, or none of them do. The credit result
// is returned unchanged (billing adapters rely on the id for refund
// linkage); a verdict is enforced as a hold, never as a failed credit.
//
// Fail CLOSED: if the lookup or a rule errors, the new commission is held
// with reason "screening_error" (outside dry-run) — a broken rule can no
// longer wave revenue through unreviewed. If the hold itself cannot be
// written, the credit rolls back and the error reaches the webhook, so
// the provider retries.
//
// A replayed delivery (ErrAlreadyProcessed) screens the commission it
// credited if no screening ever committed for it: releases before this
// one screened after the credit committed, and a crash or a failed hold
// write in between left it unheld. A screened or held commission is left
// as it is. A repair that fails is returned instead of the replay ACK.
func (e *Engine) ProcessPurchase(ctx context.Context, ev commissions.Event) (int64, error) {
	if e.screener == nil {
		return e.inner.ProcessPurchase(ctx, ev)
	}
	tp, ok := e.inner.(txPurchaser)
	if !ok {
		return e.processUnscreenedTx(ctx, ev)
	}
	cid, err := tp.ProcessPurchaseScreened(ctx, ev, e.screenFunc(ev))
	if errors.Is(err, commissions.ErrAlreadyProcessed) {
		if rerr := e.repair(ctx, tp, ev); rerr != nil {
			return 0, rerr
		}
	}
	return cid, err
}

// txPurchaser is the inner capability that runs screening inside the
// credit transaction (*commissions.Engine implements it).
type txPurchaser interface {
	ProcessPurchaseScreened(ctx context.Context, ev commissions.Event, screen commissions.ScreenFunc) (int64, error)
	ScreenCommission(ctx context.Context, commissionID int64, screen commissions.ScreenFunc) error
	CreditedCommission(ctx context.Context, source, externalID string) (int64, error)
}

var _ txPurchaser = (*commissions.Engine)(nil)

// repair screens the commission a replayed event credited, if it has
// never been screened (see ProcessPurchase).
func (e *Engine) repair(ctx context.Context, tp txPurchaser, ev commissions.Event) error {
	cid, err := tp.CreditedCommission(ctx, ev.Source, ev.ExternalID)
	if err != nil {
		return fmt.Errorf("screening: replay lookup %s:%s: %w", ev.Source, ev.ExternalID, err)
	}
	if cid <= 0 {
		return nil
	}
	if err := tp.ScreenCommission(ctx, cid, e.screenFunc(ev)); err != nil {
		return fmt.Errorf("screening: replay screen commission %d (%s:%s): %w", cid, ev.Source, ev.ExternalID, err)
	}
	return nil
}

// screenFunc screens one commission inside the transaction it is given.
// It is idempotent: a commission already screened (screened_at) or
// already held (any hold row, open or resolved) is left alone, so a
// reviewer's release is never re-opened. The transaction may be retried,
// so it derives everything from tx.
func (e *Engine) screenFunc(ev commissions.Event) commissions.ScreenFunc {
	return func(ctx context.Context, tx *ledger.Tx, cid int64) error {
		open, held, err := screenState(ctx, tx, cid)
		if err != nil || !open {
			return err
		}
		if !held {
			if err := e.screen(fraud.WithTx(ctx, tx), ev, cid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE commissions SET screened_at = ? WHERE id = ?`, time.Now().UTC(), cid); err != nil {
			return fmt.Errorf("screening: mark commission %d screened: %w", cid, err)
		}
		return nil
	}
}

// screenState reads, under the commission's row lock, whether cid still
// awaits screening (open: never screened, and pending or approved) and
// whether it has a hold row, open or resolved. Paid, cancelled and
// clawback rows are past screening.
func screenState(ctx context.Context, tx *ledger.Tx, cid int64) (open, held bool, err error) {
	var screened sql.NullString
	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT CAST(screened_at AS TEXT), status FROM commissions WHERE id = ?`+tx.ForUpdate(), cid).
		Scan(&screened, &status); err != nil {
		return false, false, fmt.Errorf("screening: commission %d: %w", cid, err)
	}
	if screened.Valid || (status != "pending" && status != "approved") {
		return false, false, nil
	}
	var holds int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM commission_holds WHERE commission_id = ?`, cid).Scan(&holds); err != nil {
		return false, false, fmt.Errorf("screening: commission %d holds: %w", cid, err)
	}
	return true, holds > 0, nil
}

// screen evaluates commission cid and fails closed. Under screenFunc, ctx
// carries the credit's transaction (fraud.WithTx): the lookup sees the
// uncommitted row and every verdict and hold commits with it. Only a hold
// that cannot be written is an error.
func (e *Engine) screen(ctx context.Context, ev commissions.Event, cid int64) error {
	now := ev.OccurredAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	q := fraud.Query{
		CommissionID: cid,
		AffiliateID:  ev.AffiliateID,
		ProgramID:    ev.CampaignID,
		Email:        ev.CustomerEmail,
		CouponCode:   ev.CouponCode,
		DryRun:       e.dryRun,
		Now:          now,
	}
	var screenErr error
	if lk, ok := e.screener.(CommissionLookup); ok {
		// The persisted row is the truth: the engine may have resolved the
		// affiliate itself (coupon / customer binding), and the click's
		// ip_hash is what makes click_velocity able to fire at all.
		cc, lerr := lk.LookupCommission(ctx, cid)
		if lerr != nil {
			screenErr = lerr
		} else {
			q.AffiliateID = cc.AffiliateID
			if cc.CampaignID != 0 {
				q.ProgramID = cc.CampaignID
			}
			q.IPHash = cc.IPHash
		}
	}
	if _, serr := e.screener.Evaluate(ctx, q); serr != nil {
		screenErr = errors.Join(screenErr, serr)
	}
	if screenErr == nil {
		return nil
	}
	log.Printf("screening: commission %d (affiliate=%d event=%s:%s): %v [fail-closed]",
		cid, q.AffiliateID, ev.Source, ev.ExternalID, screenErr)
	if e.dryRun {
		return nil
	}
	h, ok := e.screener.(Holder)
	if !ok {
		return nil
	}
	if herr := h.HoldCommission(ctx, cid, ScreeningErrorReason); herr != nil {
		return fmt.Errorf("screening: hold commission %d after screening error: %w (screening: %w)", cid, herr, screenErr)
	}
	return nil
}

// processUnscreenedTx serves an inner engine without the transactional
// capability (test doubles): it credits, then screens outside the
// credit's transaction. A hold that cannot be written is returned with
// the credit's id, so the webhook errors and the provider retries.
func (e *Engine) processUnscreenedTx(ctx context.Context, ev commissions.Event) (int64, error) {
	cid, err := e.inner.ProcessPurchase(ctx, ev)
	if err != nil || cid <= 0 {
		return cid, err
	}
	return cid, e.screen(ctx, ev, cid)
}

// ProcessRefund forwards unchanged (fraud logic for refunds is the
// clawback path, not a screening decision).
func (e *Engine) ProcessRefund(ctx context.Context, ev commissions.Event, originalCommissionID, refundedMinor, originalMinor int64) error {
	return e.inner.ProcessRefund(ctx, ev, originalCommissionID, refundedMinor, originalMinor)
}

// ProcessRefundCumulative forwards a cumulative-total refund (Lemon
// Squeezy) to the inner engine. An inner engine without the capability
// receives the cumulative value through ProcessRefund.
func (e *Engine) ProcessRefundCumulative(ctx context.Context, ev commissions.Event, originalCommissionID, cumulativeRefundedMinor, originalMinor int64) error {
	if cr, ok := e.inner.(cumulativeRefunder); ok {
		return cr.ProcessRefundCumulative(ctx, ev, originalCommissionID, cumulativeRefundedMinor, originalMinor)
	}
	return e.inner.ProcessRefund(ctx, ev, originalCommissionID, cumulativeRefundedMinor, originalMinor)
}

// CreditedCommission forwards the replay lookup billing adapters use to
// finish refund linkage (0 when the inner engine cannot answer).
func (e *Engine) CreditedCommission(ctx context.Context, source, externalID string) (int64, error) {
	if c, ok := e.inner.(creditedFinder); ok {
		return c.CreditedCommission(ctx, source, externalID)
	}
	return 0, nil
}

// BindCustomer forwards a lifetime customer binding to the inner engine
// (no-op when it cannot bind). Nothing to screen here: no money moves, and
// every commission the binding later earns passes ProcessPurchase above.
func (e *Engine) BindCustomer(ctx context.Context, customerExtID string, affiliateID, campaignID int64, method string) error {
	if b, ok := e.inner.(customerBinder); ok {
		return b.BindCustomer(ctx, customerExtID, affiliateID, campaignID, method)
	}
	return nil
}

// String identifies the decorator in logs.
func (e *Engine) String() string {
	return fmt.Sprintf("screening.Engine(dryRun=%v)", e.dryRun)
}
