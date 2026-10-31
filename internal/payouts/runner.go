// Package payouts runs the payout lifecycle: requested -> approved ->
// processing -> paid | failed. Payouts cover WHOLE commissions (INV-10);
// failed runs reverse their payouts to requested for retry.
package payouts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Runner executes payout batches.
type Runner struct {
	db  *sql.DB
	now func() time.Time

	// RailLookup asks the money rail whether a transfer for the payout
	// already landed (its reference; "" = none). Approve calls it before
	// restaging a payout whose real rail was attempted (a client-side
	// timeout can hide a transfer that succeeded). Set it whenever Approve
	// is given a real executor: without it such a payout is never
	// restaged, only flagged (ErrRailUnverified). For Stripe Connect, wrap
	// StripeConnect.Find with the payout's connected account and currency.
	RailLookup func(ctx context.Context, payoutID, amount int64) (string, error)

	// ledger is this runner's settle ledger (WithLedger). nil falls back
	// to the process-wide LedgerPoster / Ledger pair (the self-host
	// binary); a multi-tenant server sets one per shop, so no global is
	// shared between shops.
	ledger SettleLedger

	// OnSettle runs inside the settle transaction, after the payout and
	// its commissions are 'paid' and the journal is posted: rows that
	// must exist exactly when the payout is paid (a wallet code's
	// history row, a cost-line journal posted with the ctx it is given)
	// commit or roll back with the settle. An error rolls the settle back.
	OnSettle func(ctx context.Context, tx SettleExec, payoutID int64) error

	// ExportPayee, when set, runs in ClaimForExport after the payment
	// gate, under the payout's money locks: a non-nil block leaves the
	// payout out of the file (counted in Skipped, never claimed), and
	// otherwise the payee it returns replaces the affiliate's account
	// email in the file. A host with its own protected payout details
	// (the Shopify app) routes money there instead of the account email.
	ExportPayee func(ctx context.Context, payoutID, affiliateID int64, method, accountEmail string) (payee string, block *GateBlock, err error)

	// Exclude, when set, leaves an affiliate out of a batch of the given
	// method (CreateBatch): their commissions stay approved for a later
	// batch or another rail (the Shopify app: a US person without a W-9
	// is left out of cash batches, FR-55).
	Exclude func(ctx context.Context, affiliateID int64, method string) (bool, error)

	// Tax, when set, is each payout's VAT or withholding (FR-107, see
	// tax.go): the rail sends TaxSplit.Send(amount) and the settle books
	// the tax in its own journal. nil = no tax: the rail sends the amount.
	Tax TaxFunc

	dialectOnce sync.Once
	pg          bool
}

// SettleLedger is a ledger a Runner settles payouts on: Run opens the
// settle transaction and Post, called with the ctx Run hands its
// function, posts into that same transaction (*ledger.Poster and
// *ledger.PooledPoster both satisfy it).
type SettleLedger interface {
	TxRunner
	Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []ledger.Line) (int64, error)
}

// WithLedger binds the runner's own settle ledger (MO-19): every paid
// payout posts its payable -> clearing journal under SettleKey on it, in
// the settle transaction. It takes precedence over the package globals.
func (r *Runner) WithLedger(l SettleLedger) *Runner {
	r.ledger = l
	return r
}

// settlePost returns the journal poster and transaction runner this
// runner settles with (nil, nil = manual mode: no journal).
func (r *Runner) settlePost() (func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error, TxRunner) {
	if r.ledger != nil {
		l := r.ledger
		return func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error {
			_, err := l.Post(ctx, SettleKey(payoutID), "payout", fmt.Sprintf("payout %d", payoutID),
				r.now().UTC(), SettleLines(affiliateID, currency, amount))
			return err
		}, l
	}
	return LedgerPoster, Ledger
}

// NewRunner builds a Runner.
func NewRunner(db *sql.DB) *Runner {
	return &Runner{db: db, now: time.Now}
}

// PlannedPayout is one payout a batch would create: an (affiliate,
// currency) group, its lines (whole commissions plus netted clawbacks) and
// the net total the payout pays.
type PlannedPayout struct {
	AffiliateID int64  `json:"affiliate_id"`
	Currency    string `json:"currency"`
	Commissions int64  `json:"commissions"` // positive lines
	Clawbacks   int64  `json:"clawbacks"`   // netted negative lines
	Cents       int64  `json:"cents"`       // net amount paid, minor units of Currency
	Display     string `json:"display"`     // Cents for people, e.g. "$12.50", "5,000 JPY"
	lines       []int64
	amounts     []int64 // amounts[i] is lines[i]'s amount
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// plan selects what a batch pays right now. CreateBatch writes exactly
// this plan inside its transaction; Preview returns it without writing, so
// a dry run can never disagree with the real run.
func plan(ctx context.Context, q querier, th Thresholds) ([]PlannedPayout, error) {
	return planScoped(ctx, q, th, 0, "")
}

// planScoped is plan for one affiliate and currency (affiliateID 0 =
// every affiliate, every currency).
func planScoped(ctx context.Context, q querier, th Thresholds, affiliateID int64, currency string) ([]PlannedPayout, error) {
	scope, scopeK := "", ""
	var args, argsK []any
	if affiliateID != 0 {
		scope = " AND c.affiliate_id = ? AND c.currency = ?"
		scopeK = " AND k.affiliate_id = ? AND k.currency = ?"
		args = []any{affiliateID, money.Normalize(currency)}
		argsK = args
	}
	// Eligible commissions per affiliate: approved, not already in a payout.
	// Tax-doc gate (FR-17): an affiliate with any tax document still
	// 'pending' or 'rejected' is skipped wholesale this cycle — their
	// commissions stay approved and re-enter automatically once the
	// operator records 'received'/'validated'/'not_required'.
	rows, err := q.QueryContext(ctx, `
		SELECT c.affiliate_id, c.currency, c.id, c.amount
		FROM commissions c
		WHERE c.status = 'approved'
		  AND NOT EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = c.id)
		  AND NOT EXISTS (SELECT 1 FROM commission_holds ch
		                  WHERE ch.commission_id = c.id AND ch.resolved_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM tax_documents td
		                  WHERE td.affiliate_id = c.affiliate_id
		                    AND td.status IN ('pending','rejected'))
		  AND NOT EXISTS (SELECT 1 FROM affiliates af
		                  WHERE af.id = c.affiliate_id AND af.status <> 'active')`+scope+`
		ORDER BY c.affiliate_id, c.currency, c.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("payouts: eligible: %w", err)
	}
	type line struct{ comm, amount int64 }
	type group struct {
		aff      int64
		currency string
	}
	byGroup := map[group][]line{}
	var order []group
	candidate := map[int64]bool{}
	add := func(g group, l line) {
		if _, seen := byGroup[g]; !seen {
			order = append(order, g)
		}
		byGroup[g] = append(byGroup[g], l)
	}
	for rows.Next() {
		var g group
		var l line
		if err := rows.Scan(&g.aff, &g.currency, &l.comm, &l.amount); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidate[l.comm] = true
		add(g, l)
	}
	_ = rows.Close()

	// Clawbacks not yet netted into any payout, whose parent's money is
	// (or was) counted by a payout: parent paid / clawed-after-payout, or
	// parent approved and staged now or earlier. Clawbacks of cancelled
	// parents offset money that is never paid out — never netted.
	rows, err = q.QueryContext(ctx, `
		SELECT k.affiliate_id, k.currency, k.id, k.amount, p.id, p.status,
		       CASE WHEN EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = p.id)
		            THEN 1 ELSE 0 END
		FROM commissions k
		JOIN commissions p ON p.id = k.parent_commission_id
		WHERE k.amount < 0
		  AND p.status IN ('approved','paid','clawback')
		  AND NOT EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = k.id)
		  AND NOT EXISTS (SELECT 1 FROM tax_documents td
		                  WHERE td.affiliate_id = k.affiliate_id
		                    AND td.status IN ('pending','rejected'))
		  AND NOT EXISTS (SELECT 1 FROM affiliates af
		                  WHERE af.id = k.affiliate_id AND af.status <> 'active')`+scopeK+`
		ORDER BY k.affiliate_id, k.currency, k.id`, argsK...)
	if err != nil {
		return nil, fmt.Errorf("payouts: clawbacks: %w", err)
	}
	for rows.Next() {
		var g group
		var l line
		var parentID, parentStaged int64
		var parentStatus string
		if err := rows.Scan(&g.aff, &g.currency, &l.comm, &l.amount, &parentID, &parentStatus, &parentStaged); err != nil {
			_ = rows.Close()
			return nil, err
		}
		// An approved parent that is neither staged nor eligible now (held)
		// keeps its clawback until the parent itself is batched.
		if parentStatus == "approved" && parentStaged == 0 && !candidate[parentID] {
			continue
		}
		add(g, l)
	}
	_ = rows.Close()

	var out []PlannedPayout
	for _, g := range order {
		lines := byGroup[g]
		pp := PlannedPayout{AffiliateID: g.aff, Currency: g.currency}
		for _, l := range lines {
			pp.Cents += l.amount
			pp.lines = append(pp.lines, l.comm)
			pp.amounts = append(pp.amounts, l.amount)
			if l.amount < 0 {
				pp.Clawbacks++
			} else {
				pp.Commissions++
			}
		}
		if pp.Cents <= 0 || pp.Cents < th.For(g.currency) {
			continue // nothing owed net of clawbacks, or below threshold this cycle
		}
		pp.Display = money.Display(pp.Cents, pp.Currency)
		out = append(out, pp)
	}
	return out, nil
}

// Thresholds is the minimum payout per currency, in minor units of each:
// a group below its currency's minimum waits for the next cycle. Minimums
// are never converted between currencies.
type Thresholds struct {
	Default    int64            // currencies without their own entry
	ByCurrency map[string]int64 // e.g. {"JPY": 5000} = ¥5,000
	// Major, when set, is one amount in major units ("50") applied in
	// each currency by its ISO exponent (MO-9): $50.00 = 5000, ¥50 = 50,
	// 50.000 KWD = 50000. It replaces Default.
	Major string
}

// MajorThresholds is the same major-unit minimum in every currency
// (MO-9). The amount is a plain non-negative decimal ("50", "12.5");
// digits beyond a currency's exponent are dropped for that currency.
func MajorThresholds(amount string) (Thresholds, error) {
	amount = strings.TrimSpace(amount)
	whole, frac, dot := strings.Cut(amount, ".")
	if _, err := money.ParseMinor(whole, "JPY"); err != nil || strings.HasPrefix(amount, "-") || (dot && (frac == "" || strings.Trim(frac, "0123456789") != "")) {
		return Thresholds{}, fmt.Errorf("payouts: minimum %q: %w", amount, money.ErrMalformed)
	}
	return Thresholds{ByCurrency: map[string]int64{}, Major: amount}, nil
}

// majorMinor converts a major-unit decimal to minor units of currency,
// dropping fraction digits beyond the currency's exponent.
func majorMinor(amount, currency string) (int64, error) {
	if whole, frac, ok := strings.Cut(amount, "."); ok {
		if exp := money.Exponent(currency); len(frac) > exp {
			frac = frac[:exp]
		}
		amount = whole
		if frac != "" {
			amount += "." + frac
		}
	}
	return money.ParseMinor(amount, currency)
}

// Uniform is the same minor-unit minimum for every currency (the
// behaviour of a single min_threshold_cents).
func Uniform(minMinor int64) Thresholds {
	return Thresholds{Default: minMinor, ByCurrency: map[string]int64{}}
}

// For returns the minimum for one currency.
func (t Thresholds) For(currency string) int64 {
	if v, ok := t.ByCurrency[money.Normalize(currency)]; ok {
		return v
	}
	if t.Major != "" {
		if v, err := majorMinor(t.Major, currency); err == nil {
			return v
		}
	}
	return t.Default
}

// Preview returns the payouts CreateBatch would create now, writing nothing.
func (r *Runner) Preview(ctx context.Context, minThresholdMinor int64) ([]PlannedPayout, error) {
	return r.PreviewWith(ctx, Uniform(minThresholdMinor))
}

// PreviewWith is Preview with per-currency minimums.
func (r *Runner) PreviewWith(ctx context.Context, th Thresholds) ([]PlannedPayout, error) {
	return plan(ctx, r.db, th)
}

// CreateBatch is CreateBatchWith one minor-unit minimum for every currency.
func (r *Runner) CreateBatch(ctx context.Context, minThresholdMinor int64, method string) (runID int64, nPayouts int64, err error) {
	return r.CreateBatchWith(ctx, Uniform(minThresholdMinor), method)
}

// CreateBatchWith collects all approved commissions per (affiliate,
// currency) into payout rows (whole commissions only), respecting each
// currency's minimum (th, in minor units of that currency). Each payout is
// written in its commissions' currency: amounts in different currencies
// are never summed together.
//
// Clawbacks are netted: a refund clawback that reduced the affiliate's
// payable (a partial refund on an approved commission, or any refund after
// payout, which leaves a receivable) joins the batch as a negative line,
// so the payout pays what the ledger owes — never the gross. A group whose
// net is not positive is skipped; its clawbacks wait for future earnings.
// Idempotent per run: a running/aborted run's payouts are reused, not doubled.
func (r *Runner) CreateBatchWith(ctx context.Context, th Thresholds, method string) (runID int64, nPayouts int64, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("payouts: begin: %w", err)
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `INSERT INTO payout_runs (status) VALUES ('running') RETURNING id`).Scan(&runID)
	if err != nil {
		return 0, 0, fmt.Errorf("payouts: run row: %w", err)
	}

	planned, err := r.planLocked(ctx, tx, th)
	if err != nil {
		return 0, 0, err
	}
	for _, pp := range planned {
		if r.Exclude != nil {
			skip, err := r.Exclude(ctx, pp.AffiliateID, method)
			if err != nil {
				return 0, 0, fmt.Errorf("payouts: exclude aff=%d: %w", pp.AffiliateID, err)
			}
			if skip {
				continue
			}
		}
		// Ledger: payable -> payout clearing (post via caller-provided hook is
		// done at Approve; here we only stage the payout row).
		var pid int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO payouts (affiliate_id, amount, currency, method, status)
			VALUES (?, ?, ?, ?, 'requested') RETURNING id`, pp.AffiliateID, pp.Cents, pp.Currency, method).Scan(&pid); err != nil {
			return 0, 0, fmt.Errorf("payouts: insert payout aff=%d currency=%s: %w", pp.AffiliateID, pp.Currency, err)
		}
		for _, comm := range pp.lines {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO payout_lines (payout_id, commission_id) VALUES (?, ?)`,
				pid, comm); err != nil {
				return 0, 0, fmt.Errorf("payouts: line: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO payout_run_lines (run_id, payout_id) VALUES (?, ?)`,
			runID, pid); err != nil {
			return 0, 0, err
		}
		nPayouts++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("payouts: commit: %w", err)
	}
	return runID, nPayouts, nil
}

// planLocked is plan for a batch, serialized with Stage (payout
// requests, wallet redeems): on Postgres the batch takes the same
// per-(affiliate, currency) advisory lock Stage takes for every group it
// is about to stage, in sorted order, then plans again under them, so a
// concurrent request or redeem queues instead of failing the whole batch
// on uq_payout_line_commission. A group that appears only on the re-plan
// is locked and planned once more.
func (r *Runner) planLocked(ctx context.Context, tx *sql.Tx, th Thresholds) ([]PlannedPayout, error) {
	planned, err := plan(ctx, tx, th)
	if err != nil || !r.postgres(ctx) {
		return planned, err
	}
	locked := map[string]bool{}
	for attempt := 0; attempt < 4; attempt++ {
		var keys []string
		for _, pp := range planned {
			k := fmt.Sprintf("%d:%s", pp.AffiliateID, money.Normalize(pp.Currency))
			if !locked[k] {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return planned, nil
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx,
				`SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':payout-stage:' || ?, 0))`, k); err != nil {
				return nil, fmt.Errorf("payouts: batch stage lock: %w", err)
			}
			locked[k] = true
		}
		if planned, err = plan(ctx, tx, th); err != nil {
			return nil, err
		}
	}
	return planned, nil
}

// ErrRestaged reports a payout that Approve failed and released because its
// staged amount no longer matches what is owed (a commission left, or a
// clawback landed, after staging). payouts.amount is immutable
// (trg_payout_money_immutable), so the payout is never re-priced in place:
// the member commissions return to the pool and the next CreateBatch pays
// the correct net amount.
var ErrRestaged = errors.New("payouts: payout released for restaging")

// ErrAlreadyPaid reports an Approve of a payout that is already 'paid'.
// No rail runs and no journal posts: approving twice is a no-op.
var ErrAlreadyPaid = errors.New("payouts: payout already paid")

// ErrReferenceRequired: MarkPaid without a payment reference.
var ErrReferenceRequired = errors.New("payouts: a payment reference is required to mark a payout paid")

// ErrNotApproved: MarkPaid of a payout finance has not approved.
var ErrNotApproved = errors.New("payouts: payout is not approved")

// ErrNotReleasable: Release of a payout that may already have moved
// money (a rail was attempted, or it is in a PayPal/Wise file).
var ErrNotReleasable = errors.New("payouts: payout cannot be released")

// ErrBlocked: the payment gate refuses the payout for now (an open fraud
// hold, a tax document, a paused affiliate). The payout stays requested
// and can be approved again once the cause is cleared (422).
var ErrBlocked = errors.New("payouts: payout blocked")

// ErrInFlight: another approval (on this host or another) is moving the
// payout right now; this one changed nothing (409).
var ErrInFlight = errors.New("payouts: payout in flight")

// release frees a payout that has NOT executed: its lines are deleted
// (the member commissions re-enter the next batch) and it moves to
// 'failed', in one transaction. The delete runs first, while the payout is
// still 'processing': the guard trigger refuses to touch the lines of a
// payout in any other state. nil = released.
func (r *Runner) release(ctx context.Context, payoutID int64, why string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("payouts: release %d (%s): begin: %w", payoutID, why, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM payout_lines WHERE payout_id = ?`, payoutID); err != nil {
		return fmt.Errorf("payouts: release %d (%s): release lines: %w", payoutID, why, err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE payouts SET status='failed', attention=? WHERE id = ? AND status = 'processing'`, why, payoutID)
	if err != nil {
		return fmt.Errorf("payouts: release %d (%s): fail: %w", payoutID, why, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("payouts: release %d (%s): payout left processing", payoutID, why)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("payouts: release %d (%s): commit: %w", payoutID, why, err)
	}
	return nil
}

// restage is release for Approve and Authorize: the caller learns that
// the payout it was asked to pay is gone (ErrRestaged).
func (r *Runner) restage(ctx context.Context, payoutID int64, why string) error {
	if err := r.release(ctx, payoutID, why); err != nil {
		return err
	}
	return fmt.Errorf("%w: payout %d: %s", ErrRestaged, payoutID, why)
}

// Release cancels a staged payout before any money moved (a declined
// payout request, FR-106 AC3): its commissions return to the pool,
// still 'approved', and the payout ends 'failed'. A payout a real rail
// was attempted for, or one listed in a PayPal/Wise file, is refused
// (ErrNotReleasable): money may have left. nil = released.
func (r *Runner) Release(ctx context.Context, payoutID int64, why string) error {
	unlock, err := r.lockPayout(ctx, payoutID)
	if err != nil {
		return err
	}
	defer unlock()
	var status string
	var attempted, exported int
	if err := r.db.QueryRowContext(ctx, `
		SELECT status, CASE WHEN rail_attempted_at IS NULL THEN 0 ELSE 1 END,
		       CASE WHEN exported_at IS NULL THEN 0 ELSE 1 END
		FROM payouts WHERE id = ?`, payoutID).Scan(&status, &attempted, &exported); err != nil {
		return fmt.Errorf("payouts: release lookup %d: %w", payoutID, err)
	}
	if status != "requested" && status != "approved" {
		return fmt.Errorf("%w: payout %d is %s", ErrNotReleasable, payoutID, status)
	}
	if attempted == 1 || exported == 1 {
		return fmt.Errorf("%w: payout %d may have been paid (rail attempted or exported)", ErrNotReleasable, payoutID)
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE payouts SET status='processing' WHERE id = ? AND status = ?`, payoutID, status)
	if err != nil {
		return fmt.Errorf("payouts: release %d: %w", payoutID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("payouts: payout %d changed state during release", payoutID)
	}
	return r.release(ctx, payoutID, why)
}

// Authorize is finance's approval of a staged payout (MO-20): requested
// -> approved, nothing paid and no journal posted. The payment gate runs
// first, under the payout's and its commissions' locks: a hold, a tax
// document or a paused affiliate refuses (the payout stays requested); a
// member that left 'approved' or a clawback that landed after staging
// releases the payout (ErrRestaged), so approval never covers money that
// is no longer owed. Authorizing an approved payout again is a no-op.
// The money moves later, by MarkPaid (manual rails) or ApproveRail.
func (r *Runner) Authorize(ctx context.Context, payoutID int64) error {
	unlock, err := r.lockPayout(ctx, payoutID)
	if err != nil {
		return err
	}
	defer unlock()
	var affiliateID int64
	var status, currency string
	var exported, attempted int
	if err := r.db.QueryRowContext(ctx, `
		SELECT affiliate_id, status, currency,
		       CASE WHEN exported_at IS NULL THEN 0 ELSE 1 END,
		       CASE WHEN rail_attempted_at IS NULL THEN 0 ELSE 1 END
		FROM payouts WHERE id = ?`, payoutID).
		Scan(&affiliateID, &status, &currency, &exported, &attempted); err != nil {
		return fmt.Errorf("payouts: authorize lookup %d: %w", payoutID, err)
	}
	switch status {
	case "approved":
		return nil
	case "paid":
		return fmt.Errorf("%w: payout %d", ErrAlreadyPaid, payoutID)
	case "requested":
	default:
		return fmt.Errorf("%w: payout %d in state %s, want requested", ErrInFlight, payoutID, status)
	}
	block, err := r.gate(ctx, payoutID, affiliateID, currency)
	if err != nil {
		return err
	}
	if block != nil {
		switch {
		case block.Kind == GateHold || block.Kind == GateTaxDoc || block.Kind == GatePaused:
			return fmt.Errorf("%w: payout %d: %s", ErrBlocked, payoutID, block.Reason)
		case exported == 1 || attempted == 1:
			_, _ = r.db.ExecContext(ctx, `UPDATE payouts SET attention=? WHERE id = ?`, block.Reason, payoutID)
			return fmt.Errorf("%w: payout %d: %s", ErrNeedsAttention, payoutID, block.Reason)
		}
		res, err := r.db.ExecContext(ctx,
			`UPDATE payouts SET status='processing' WHERE id = ? AND status='requested'`, payoutID)
		if err != nil {
			return fmt.Errorf("payouts: authorize %d: %w", payoutID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: payout %d no longer requested", ErrInFlight, payoutID)
		}
		return r.restage(ctx, payoutID, block.Reason)
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE payouts SET status='approved' WHERE id = ? AND status='requested'`, payoutID)
	if err != nil {
		return fmt.Errorf("payouts: authorize %d: %w", payoutID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: payout %d no longer requested", ErrInFlight, payoutID)
	}
	return nil
}

// MarkPaid settles an approved payout that finance paid outside the app
// (manual rails, FR-51): the reference (bank transfer id, Venmo
// receipt) is required and stored as the payout's provider_ref, and the
// settle runs exactly like Approve's: gate re-checked under the locks,
// the payable -> clearing journal, the payout and its commissions to
// 'paid', in one transaction. No rail runs.
func (r *Runner) MarkPaid(ctx context.Context, payoutID int64, reference string) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ErrReferenceRequired
	}
	return r.approve(ctx, payoutID, nil, reference)
}

// RailExec executes a payout on a money rail and returns the provider's
// reference for the transfer (a PayPal batch id, a Shopify store credit
// transaction GID). It must be idempotent per payout id where the rail
// allows it (MO-21): the payout id is the rail's idempotency key.
type RailExec func(ctx context.Context, payoutID, amount int64) (ref string, err error)

// ApproveRail approves and executes a payout on a real rail, storing the
// rail's reference on the payout. A payout whose rail was attempted
// before (rail_attempted_at set: an earlier attempt timed out or failed
// after sending) is never sent again blindly: RailLookup is asked first;
// a transfer it finds settles the payout as paid, and without a lookup
// (or when it fails) the payout stays requested with attention set
// (ErrRailUnverified). Only a lookup that finds nothing re-executes.
func (r *Runner) ApproveRail(ctx context.Context, payoutID int64, exec RailExec) error {
	if exec == nil {
		return errors.New("payouts: ApproveRail needs a rail executor")
	}
	return r.approve(ctx, payoutID, exec, "")
}

// Approve flips a payout to processing and (optionally, via hook) executes
// the transfer. Hook failure marks the payout failed (retryable).
//
// Money safety (attack 1b/10): the whole rail+settle sequence runs under
// the payout's moneylock PLUS every member commission's lock — the same
// per-commission locks fraud.Uphold, refunds and maturation take — so an
// uphold racing Approve is fully serialized, never interleaved mid-settle.
// Every member's status is re-checked INSIDE the locks before any money
// moves: a commission cancelled first is never paid, and one paid first is
// refused by Uphold (ErrAlreadyPaid: the refund flow claws it back). The
// settle itself (journal, payout, commissions) is one ledger transaction
// that row-locks the payout and its commissions on Postgres; the moneylocks
// are in-process only, so across replicas the rail call is covered by the
// gate, Uphold's ErrPayoutInFlight and the payable overdraft guard.
//
// The checks are gate, the same function ClaimForExport runs before a
// payout goes into a PayPal/Wise file. An exported payout is approved
// like any other (the gate is re-checked), but it never runs a rail and
// is never restaged: if the gate now blocks it, it is flagged
// (ErrNeedsAttention, payouts.attention set) and stays claimed.
//
// A payout may be 'requested' or finance-'approved' (Authorize).
func (r *Runner) Approve(ctx context.Context, payoutID int64, execute func(payoutID int64, amount int64) error) error {
	var exec RailExec
	if execute != nil {
		exec = func(_ context.Context, id, amount int64) (string, error) { return "", execute(id, amount) }
	}
	return r.approve(ctx, payoutID, exec, "")
}

// approve is Approve, ApproveRail and MarkPaid (manualRef != "": an
// approved payout settled with that reference and no rail).
func (r *Runner) approve(ctx context.Context, payoutID int64, exec RailExec, manualRef string) error {
	// Lock the payout and all its commissions (LockAll orders the set —
	// deadlock-safe).
	unlock, err := r.lockPayout(ctx, payoutID)
	if err != nil {
		return err
	}
	defer unlock()

	var amount, affiliateID int64
	var status, currency string
	var exported bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT amount, affiliate_id, status, currency,
		        CASE WHEN exported_at IS NULL THEN 0 ELSE 1 END
		 FROM payouts WHERE id = ?`, payoutID).
		Scan(&amount, &affiliateID, &status, &currency, &exported); err != nil {
		return fmt.Errorf("payouts: lookup: %w", err)
	}
	if status == "paid" {
		// Checked under the payout lock: a second Approve (double click,
		// client retry) that queued behind the first lands here after it
		// finished. Nothing moves; callers may treat this as success.
		return fmt.Errorf("%w: payout %d", ErrAlreadyPaid, payoutID)
	}
	if manualRef != "" && status != "approved" {
		return fmt.Errorf("%w: payout %d is %s (approve it first)", ErrNotApproved, payoutID, status)
	}
	if status != "requested" && status != "approved" {
		return fmt.Errorf("%w: payout %d in state %s, want requested or approved", ErrInFlight, payoutID, status)
	}
	// CAS into processing: exactly one Approve wins.
	res, err := r.db.ExecContext(ctx,
		`UPDATE payouts SET status='processing' WHERE id = ? AND status = ?`, payoutID, status)
	if err != nil {
		return fmt.Errorf("payouts: processing: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: payout %d no longer %s", ErrInFlight, payoutID, status)
	}
	block, err := r.gate(ctx, payoutID, affiliateID, currency)
	if err != nil {
		_, _ = r.db.ExecContext(ctx,
			`UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
		return err
	}
	if block != nil && exported {
		// The payout is in a PayPal/Wise file the operator may already
		// have paid. Restaging would release its commissions into a new
		// run and pay them twice, so it stays claimed ('requested', lines
		// kept) and is flagged for the operator instead. Approve again
		// once the cause is cleared (hold resolved, tax doc received);
		// a commission that left 'approved' or a clawback that landed
		// after export needs the operator to reconcile by hand.
		_, _ = r.db.ExecContext(ctx,
			`UPDATE payouts SET status='requested', attention=? WHERE id = ?`, block.Reason, payoutID)
		return fmt.Errorf("%w: payout %d: %s", ErrNeedsAttention, payoutID, block.Reason)
	}
	if block != nil {
		switch block.Kind {
		case GateHold, GateTaxDoc, GatePaused:
			// F5 / FR-17: retryable. The payout stays 'requested' and
			// re-Approves once the hold is resolved or the doc cleared.
			_, _ = r.db.ExecContext(ctx,
				`UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
			return fmt.Errorf("%w: payout %d: %s", ErrBlocked, payoutID, block.Reason)
		default:
			// Attack-10 pins: payouts.amount is immutable, so the payout
			// is released and the survivors re-batch at the right amount —
			// unless a real rail already ran for it. A transfer that timed
			// out client-side may have landed at the provider; restaging
			// would send the survivors again under a new payout id.
			sent := amount // what a rail that ran moved (FR-107)
			if v, err := r.sendAmount(ctx, payoutID, affiliateID, currency, amount); err == nil {
				sent = v
			}
			ref, err := r.railLanded(ctx, payoutID, sent)
			if err != nil {
				_, _ = r.db.ExecContext(ctx,
					`UPDATE payouts SET status='requested', attention=? WHERE id = ?`, err.Error(), payoutID)
				return fmt.Errorf("payouts: payout %d: %s: %w", payoutID, block.Reason, err)
			}
			if ref == "" {
				return r.restage(ctx, payoutID, block.Reason)
			}
			// The money left at the staged amount: settle it as executed
			// and leave the block for the operator to reconcile.
			return r.settlePaid(ctx, payoutID, affiliateID, currency, amount, "",
				fmt.Sprintf("paid at the provider (%s) before approval was blocked: %s; reconcile", ref, block.Reason))
		}
	}
	if exported {
		// Paid from the file: never run a rail a second time.
		exec = nil
	}
	if exec == nil && !exported {
		// A manual approval or Mark Paid of a payout a real rail was
		// attempted for asks the rail first, exactly as a rail retry does:
		// a transfer that timed out may have landed, and settling it by
		// hand would pay it twice. Without a lookup it fails closed
		// (ErrRailUnverified); a found transfer settles as executed.
		var attempted int
		if err := r.db.QueryRowContext(ctx,
			`SELECT CASE WHEN rail_attempted_at IS NULL THEN 0 ELSE 1 END FROM payouts WHERE id = ?`, payoutID).
			Scan(&attempted); err != nil {
			_, _ = r.db.ExecContext(ctx, `UPDATE payouts SET status = ? WHERE id = ?`, status, payoutID)
			return fmt.Errorf("payouts: rail attempt lookup %d: %w", payoutID, err)
		}
		send := amount
		if v, err := r.sendAmount(ctx, payoutID, affiliateID, currency, amount); err == nil {
			send = v
		}
		var ref string
		var err error
		if attempted == 1 {
			ref, err = r.railLanded(ctx, payoutID, send)
		}
		if err != nil {
			_, _ = r.db.ExecContext(ctx,
				`UPDATE payouts SET status = ?, attention=? WHERE id = ?`, status, err.Error(), payoutID)
			return fmt.Errorf("payouts: payout %d: %w", payoutID, err)
		}
		if ref != "" {
			return r.settlePaid(ctx, payoutID, affiliateID, currency, amount, "", "")
		}
	}
	if manualRef != "" {
		if _, err := r.db.ExecContext(ctx,
			`UPDATE payouts SET provider_ref = ? WHERE id = ?`, manualRef, payoutID); err != nil {
			_, _ = r.db.ExecContext(ctx, `UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
			return fmt.Errorf("payouts: record reference %d: %w", payoutID, err)
		}
	}
	manualRail := exec == nil
	if !manualRail {
		// FR-107: the rail moves the amount after VAT or withholding.
		send, err := r.sendAmount(ctx, payoutID, affiliateID, currency, amount)
		if err != nil {
			_, _ = r.db.ExecContext(ctx, `UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
			return err
		}
		// MO-21: a rail attempted before is asked before it is sent
		// again; without a lookup the payout fails closed.
		ref, err := r.railLanded(ctx, payoutID, send)
		if err != nil {
			_, _ = r.db.ExecContext(ctx,
				`UPDATE payouts SET status='requested', attention=? WHERE id = ?`, err.Error(), payoutID)
			return fmt.Errorf("payouts: payout %d: %w", payoutID, err)
		}
		if ref != "" {
			return r.settlePaid(ctx, payoutID, affiliateID, currency, amount, "", "")
		}
		var priorAttempt bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT CASE WHEN rail_attempted_at IS NULL THEN 0 ELSE 1 END FROM payouts WHERE id = ?`, payoutID).
			Scan(&priorAttempt); err != nil {
			_, _ = r.db.ExecContext(ctx, `UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
			return fmt.Errorf("payouts: rail attempt lookup %d: %w", payoutID, err)
		}
		if _, err := r.db.ExecContext(ctx,
			`UPDATE payouts SET rail_attempted_at = COALESCE(rail_attempted_at, ?) WHERE id = ?`,
			r.now().UTC().Format(time.RFC3339), payoutID); err != nil {
			// Never hand a payout to a real rail without the marker that stops
			// a later restage from paying it twice.
			_, _ = r.db.ExecContext(ctx,
				`UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
			return fmt.Errorf("payouts: mark rail attempt %d: %w", payoutID, err)
		}
		ref, err = exec(ctx, payoutID, send)
		if err != nil {
			// Failed: reverse to requested for retry. Money normally never
			// left payable, but a timeout may hide a transfer that landed:
			// rail_attempted_at makes the retry and any restage ask the
			// rail first. A rail that refused before sending anything
			// (ErrNotSent) leaves no such doubt, so the marker this attempt
			// set is cleared; one an earlier attempt set stays.
			if errors.Is(err, ErrNotSent) && !priorAttempt {
				_, _ = r.db.ExecContext(ctx,
					`UPDATE payouts SET status='requested', rail_attempted_at = NULL WHERE id = ?`, payoutID)
			} else {
				_, _ = r.db.ExecContext(ctx,
					`UPDATE payouts SET status='requested' WHERE id = ?`, payoutID)
			}
			return fmt.Errorf("payouts: execute %d: %w", payoutID, err)
		}
		if ref != "" {
			// Recorded before the settle: a settle that fails leaves the
			// payout 'failed' with the provider's reference on it.
			_, _ = r.db.ExecContext(ctx, `
				UPDATE payouts SET provider_ref = ?
				WHERE id = ? AND (provider_ref IS NULL OR provider_ref = '')`, ref, payoutID)
		}
	}
	// A manual settle that fails moved no money: the payout returns to
	// the state it was approved from ('approved' for Mark Paid), so
	// finance's approval is not lost. A rail that ran never returns.
	restore := ""
	if manualRail {
		restore = status
	}
	return r.settlePaid(ctx, payoutID, affiliateID, currency, amount, restore, "")
}

// ErrNotSent marks a rail refusal made before anything was sent to the
// provider (a pre-flight check: a missing permission, no customer, a
// cap). A RailExec wraps it so the attempt leaves no "may have been
// paid" marker behind and the payout can be paid another way.
var ErrNotSent = errors.New("payouts: rail refused before sending")

// ErrRailUnverified reports a payout the gate would restage whose real
// rail was attempted, when the rail cannot say whether the transfer
// landed (no RailLookup wired, or the lookup failed). The payout stays
// 'requested' with attention set; nothing is restaged.
var ErrRailUnverified = errors.New("payouts: rail attempted; transfer not verified")

// railLanded reports the provider reference of a transfer a real rail
// already made for the payout ("" = none, or the rail never ran). It is
// asked before any restage.
func (r *Runner) railLanded(ctx context.Context, payoutID, amount int64) (string, error) {
	var attempted int
	var ref sql.NullString
	if err := r.db.QueryRowContext(ctx, `
		SELECT CASE WHEN rail_attempted_at IS NULL THEN 0 ELSE 1 END, provider_ref
		FROM payouts WHERE id = ?`, payoutID).Scan(&attempted, &ref); err != nil {
		return "", fmt.Errorf("payouts: rail attempt lookup: %w", err)
	}
	if ref.String != "" {
		return ref.String, nil
	}
	if attempted == 0 {
		return "", nil
	}
	if r.RailLookup == nil {
		return "", fmt.Errorf("%w: payout %d has no rail lookup", ErrRailUnverified, payoutID)
	}
	found, err := r.RailLookup(ctx, payoutID, amount)
	if err != nil {
		return "", fmt.Errorf("%w: payout %d: %w", ErrRailUnverified, payoutID, err)
	}
	if found != "" {
		_, _ = r.db.ExecContext(ctx, `
			UPDATE payouts SET provider_ref = ?
			WHERE id = ? AND (provider_ref IS NULL OR provider_ref = '')`, found, payoutID)
	}
	return found, nil
}

// settlePaid settles an executed payout: the ledger journal, the payout
// and its commissions to 'paid', in ONE transaction. attention ("" =
// clear) is left on the paid payout for the operator. restore is the
// status a failed settle returns the payout to when no money moved (a
// manual rail: 'requested' or 'approved'); "" = a real rail ran.
func (r *Runner) settlePaid(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64, restore, attention string) error {
	manualRail := restore != ""
	if err := r.settle(ctx, payoutID, affiliateID, currency, amount, attention); err != nil {
		// Nothing of the settle landed. F6: never strand 'processing'. If a
		// REAL rail executed, money has left — reverting to 'requested'
		// would let a re-Approve pay twice, so the payout goes to 'failed'
		// (terminal for the rail; operator completes the journal manually —
		// the idem key payout:<id> makes that safe). With no executor
		// (manual rail) nothing was paid, so retry from 'requested'; a
		// journal that already landed is absorbed by the verified-duplicate
		// rule in settleLedger.
		//
		// EXCEPT ErrSettleKeyConflict: that is never transient. The settle
		// key belongs to a different journal and the ledger is append-only
		// (UPDATE/DELETE refused by guards), so no retry can ever succeed —
		// 'requested' would loop a scheduled runner forever. Terminal
		// 'failed' with attention set; the operator reconciles by hand.
		// Other failures keep any attention note already on the payout.
		newStatus := "failed"
		var note any // nil = leave attention as it is
		switch {
		case errors.Is(err, ErrSettleKeyConflict):
			note = err.Error()
		case errors.Is(err, ErrSettleMembersChanged):
			note = err.Error()
			if manualRail {
				newStatus = restore
			}
		case attention != "":
			note = attention + "; settle failed: " + err.Error()
		case manualRail:
			newStatus = restore
		}
		_, _ = r.db.ExecContext(ctx,
			`UPDATE payouts SET status = ?, attention = COALESCE(?, attention) WHERE id = ?`,
			newStatus, note, payoutID)
		return fmt.Errorf("payouts: settle %d: %w", payoutID, err)
	}
	return nil
}

// SettleExec is the statement surface OnSettle gets.
type SettleExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// settleQ is the statement surface settle runs on: a ledger.Tx when the
// ledger is wired, a pool transaction in manual mode.
type settleQ interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// settle runs the settle transaction: on the ledger connection (Ledger)
// when a journal posts, so the journal and the status rows commit
// together; in a pool transaction in manual mode (no LedgerPoster), where
// only status rows change.
func (r *Runner) settle(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64, attention string) error {
	post, run := r.settlePost()
	if post == nil {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer tx.Rollback()
		if err := r.settleTx(ctx, tx, "", nil, payoutID, affiliateID, currency, amount, attention); err != nil {
			return err
		}
		return tx.Commit()
	}
	if run == nil {
		return errors.New("LedgerPoster is wired without Ledger: the settle journal cannot commit with the payout")
	}
	return run.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		return r.settleTx(ctx, tx, tx.ForUpdate(), post, payoutID, affiliateID, currency, amount, attention)
	})
}

// settleTx locks the payout and its member commissions (forUpdate: the
// row-lock suffix, "" where the transaction already serializes writers),
// posts the journal and flips the rows to 'paid'.
func (r *Runner) settleTx(ctx context.Context, q settleQ, forUpdate string, post func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error, payoutID, affiliateID int64, currency string, amount int64, attention string) error {
	var status string
	if err := q.QueryRowContext(ctx,
		`SELECT status FROM payouts WHERE id = ?`+forUpdate, payoutID).Scan(&status); err != nil {
		return fmt.Errorf("lock payout: %w", err)
	}
	if status != "processing" {
		return fmt.Errorf("payout left processing (%s) before its settle", status)
	}
	// Member commissions in id order (one lock order for every replica):
	// a refund or uphold of one of them waits for the settle, then sees it
	// paid.
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM commissions
		WHERE id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)
		ORDER BY id`+forUpdate, payoutID)
	if err != nil {
		return fmt.Errorf("lock commissions: %w", err)
	}
	for rows.Next() {
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lock commissions: %w", err)
	}
	// Re-checked under the row locks, before any money posts. A settle
	// that carries attention is the "paid at the provider before the
	// block" path: the money left; the block is the operator's to
	// reconcile.
	if attention == "" {
		if err := recheckMembers(ctx, q, payoutID); err != nil {
			return err
		}
	}

	// Paid: ledger payable -> payout_clearing per whole commissions.
	if err := settleLedger(ctx, q, post, payoutID, affiliateID, currency, amount); err != nil {
		return err
	}
	// FR-107: VAT on top or withholding books apart from commission.
	if post != nil {
		if err := r.postTax(ctx, q, payoutID, affiliateID, currency, amount, r.now().UTC()); err != nil {
			return err
		}
	}
	var note any // NULL clears attention
	if attention != "" {
		note = attention
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE payouts SET status='paid', processed_at=?, attention=? WHERE id = ?`,
		r.now().UTC().Format(time.RFC3339), note, payoutID); err != nil {
		return fmt.Errorf("paid: %w", err)
	}
	// Commissions -> paid, GUARDED: only approved rows transition. A
	// cancelled commission can never be resurrected into 'paid' here
	// (netted clawback rows stay 'cancelled'; their payout line records
	// that they were netted).
	if _, err := q.ExecContext(ctx, `
		UPDATE commissions SET status='paid', paid_at=?
		WHERE id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)
		  AND status = 'approved'`,
		r.now().UTC().Format(time.RFC3339), payoutID); err != nil {
		return fmt.Errorf("mark commissions: %w", err)
	}
	if r.OnSettle != nil {
		if err := r.OnSettle(ctx, q, payoutID); err != nil {
			return fmt.Errorf("on settle: %w", err)
		}
	}
	return nil
}

// recheckMembers refuses a settle whose member left 'approved' or whose
// member gained a clawback after the gate ran: state committed by a
// writer that did not take the in-process money locks (another
// replica) is caught under the settle's row locks.
func recheckMembers(ctx context.Context, q settleQ, payoutID int64) error {
	var changed, unnetted int64
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commissions c JOIN payout_lines pl ON pl.commission_id = c.id
		WHERE pl.payout_id = ? AND c.amount > 0 AND c.status <> 'approved'`, payoutID).Scan(&changed); err != nil {
		return fmt.Errorf("re-check members: %w", err)
	}
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commissions k
		WHERE k.amount < 0
		  AND k.parent_commission_id IN (SELECT commission_id FROM payout_lines WHERE payout_id = ?)
		  AND NOT EXISTS (SELECT 1 FROM payout_lines pl WHERE pl.commission_id = k.id)`, payoutID).Scan(&unnetted); err != nil {
		return fmt.Errorf("re-check clawbacks: %w", err)
	}
	if changed > 0 || unnetted > 0 {
		return fmt.Errorf("%w: %d member(s) left approved, %d clawback(s) landed after the gate; reconcile",
			ErrSettleMembersChanged, changed, unnetted)
	}
	return nil
}

// SettleKey is the ledger idempotency key of a payout's settle journal
// (payable -> payout clearing). LedgerPoster implementations must post
// under exactly this key: settleLedger verifies a duplicate against it.
func SettleKey(payoutID int64) string { return fmt.Sprintf("payout:%d", payoutID) }

// SettleLines are the settle journal's lines: the payout's amount off the
// affiliate's payable in the payout's currency, into that currency's
// payout clearing. A payout never touches another currency's balance.
func SettleLines(affiliateID int64, currency string, amount int64) []ledger.Line {
	return []ledger.Line{
		{Account: ledger.Payable(affiliateID, currency), Amount: amount},
		{Account: ledger.Clearing(currency), Amount: -amount},
	}
}

// LedgerPoster posts the payout journal (payable -> clearing) under
// SettleKey(payoutID). Wired by main; nil poster = manual-mode (no auto
// ledger post). It is called inside Ledger.Run with the ctx Run hands
// it: a ledger.Poster on the same connection posts into that transaction.
var LedgerPoster func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error

// TxRunner runs a ledger transaction (*ledger.Poster satisfies it).
type TxRunner interface {
	Run(ctx context.Context, fn func(ctx context.Context, tx *ledger.Tx) error) error
}

// Ledger is the ledger the settle transaction runs on; required whenever
// LedgerPoster is set (wired by main next to it, on the same poster), so
// the settle journal commits with the payout and commission rows.
var Ledger TxRunner

// ErrSettleMembersChanged: under the settle's row locks a member
// commission is no longer 'approved', or a clawback of a member landed
// after the gate. Nothing posts.
var ErrSettleMembersChanged = errors.New("payouts: payout members changed before the settle")

// ErrSettleKeyConflict: the payout's settle key already belongs to a
// journal that did NOT move this payout's money (a reused payout id after a
// restore, or a hand-posted journal). Treating it as settled would mark the
// payout paid while the affiliate payable still owes the amount.
var ErrSettleKeyConflict = errors.New("payouts: settle key taken by a different journal")

// settleLedger posts the payout journal through the poster hook.
//
// A duplicate idem key means a journal under THIS payout's settle key
// already exists — normally a previous attempt that committed and crashed
// before the status flip (possible only before settles were atomic, or
// when the operator posted it by hand), which is success. It is accepted
// only after checking that the journal really moved `amount` off this
// affiliate's payable; anything else is ErrSettleKeyConflict and the
// payout is not marked paid.
func settleLedger(ctx context.Context, q settleQ, post func(ctx context.Context, payoutID, affiliateID int64, currency string, amount int64) error, payoutID, affiliateID int64, currency string, amount int64) error {
	if post == nil {
		return nil
	}
	err := post(ctx, payoutID, affiliateID, currency, amount)
	if !errors.Is(err, ledger.ErrDuplicate) {
		return err
	}
	var moved int64
	if qerr := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(le.amount), 0)
		FROM journal_entries je
		JOIN ledger_entries le ON le.journal_id = je.id
		JOIN ledger_accounts la ON la.id = le.account_id
		WHERE je.idem_key = ? AND la.code = ?`,
		SettleKey(payoutID), ledger.Payable(affiliateID, currency)).Scan(&moved); qerr != nil {
		return fmt.Errorf("verify settle journal: %w", qerr)
	}
	if moved != amount {
		return fmt.Errorf("%w: payout %d key %s moved %d off %s, want %d",
			ErrSettleKeyConflict, payoutID, SettleKey(payoutID), moved, ledger.Payable(affiliateID, currency), amount)
	}
	return nil
}

// CompleteRun marks a run finished.
func (r *Runner) CompleteRun(ctx context.Context, runID int64, note string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payout_runs SET status='completed', finished_at=?, note=? WHERE id=?`,
		r.now().UTC().Format(time.RFC3339), note, runID)
	return err
}
