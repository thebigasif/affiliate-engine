// Package commissions computes and records commissions from conversion
// events, posts them to the ledger, and manages maturation + clawbacks.
package commissions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/bpmath"
	"github.com/thebigasif/affiliate-engine/internal/dbutil"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/money"
	"github.com/thebigasif/affiliate-engine/internal/moneylock"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// Event is a normalized conversion/billing event from any adapter.
type Event struct {
	Source        string // "stripe", "paddle", "api", ...
	ExternalID    string // provider event/charge/invoice id (idempotency)
	EventType     string // "purchase", "renewal", "refund", "usage"
	AmountMinor   int64  // gross commissionable amount, minor units
	Currency      string
	CustomerExtID string // provider customer id (lifetime binding)
	CustomerEmail string // buyer email (fraud: self-referral + payment fingerprint)
	ClickToken    string // signed attribution token (may be empty)
	ClickID       string // resolved originating click id (postback {click_id}; "" = unknown)
	OrderID       string // merchant order reference (postback {order_id}/{txid}; "" = unknown)
	CouponCode    string // may be empty
	ProductKey    string // product/plan identifier for per-product rates (FR-12; "" = any)
	Country       string // ISO-3166 alpha-2 conversion geo if the billing event carries it ("" = untagged)
	AffiliateID   int64  // may be 0 if attribution must resolve
	// Method is how the caller resolved AffiliateID: the attribution
	// ladder's rung ("token", "coupon", "customer"). It is stored as the
	// commission's attribution_method, which method-based fraud rules
	// (coupon_hijack) read. Empty with AffiliateID set = "explicit".
	Method     string
	CampaignID int64
	OccurredAt time.Time

	// FixedAmount, when > 0, IS the commission: rule resolution is
	// skipped and this amount is credited directly (the Shopify fold's
	// royalty kind, FR-87, whose rate comes from royalty_links). All
	// other purchase discipline applies unchanged.
	FixedAmount int64

	// Reason is the plain-language cause of a refund event's clawback
	// ("refund", "order cancelled", "order deleted", "dispute lost"),
	// recorded on the clawback row so the affiliate sees why a
	// commission was reduced (PRD 6.7). Empty for purchases.
	Reason string `json:",omitempty"`
}

// Engine computes commissions and drives the ledger + status lifecycle.
type Engine struct {
	db     *sql.DB
	poster Poster
	now    func() time.Time

	// MLMBVRecorder records business volume for the MLM trees when a
	// commission credits (optional; nil = MLM disabled). Fail-open by
	// design — the reconciler self-heals counter drift. currency is the
	// commission's; the recorder decides what counts (mlm.RecordBV: the
	// program currency only).
	MLMBV func(ctx context.Context, affiliateID int64, day int64, currency string, bv int64) error

	// HoldFloor, when set, is the shortest hold any commission this
	// engine credits may get, in days (the Shopify app's FR-33 refund
	// window floor). matures_at = now + max(program hold, floor). nil =
	// no floor (self-host: the program hold as configured).
	HoldFloor func(ctx context.Context) int
}

// Poster is the ledger interface (satisfied by *ledger.Poster).
//
// Run is the atomicity primitive every money path uses: the journal and
// the domain rows it pays for commit in one transaction. Inside it the
// engine posts through Post with the ctx Run hands it (the post joins the
// transaction), so a wrapper around Post still sees every journal.
type Poster interface {
	Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []ledger.Line) (int64, error)
	AccountBalance(ctx context.Context, code string) (int64, error)
	Run(ctx context.Context, fn func(ctx context.Context, tx *ledger.Tx) error) error
}

// clawbackPoster is the optional ledger capability refund clawbacks use: a
// post that may take an affiliate payable positive (the affiliate owes us
// after a post-payout refund; see ledger.Poster.PostClawback). A Poster
// without it falls back to Post and its overdraft guard.
type clawbackPoster interface {
	PostClawback(ctx context.Context, idemKey, source, description string, effective time.Time, lines []ledger.Line) (int64, error)
}

var _ clawbackPoster = (*ledger.Poster)(nil)

// Rule is a commission rule resolved at credit time (snapshotted).
type Rule struct {
	RateType  string // "percent" | "fixed"
	RateValue int64  // percent: basis points (100 = 1%); fixed: minor units
	Origin    string // "campaign_default" | "affiliate_override" | "affiliate_product" | "campaign_product" | "coupon" | "volume_tier"
	// Currency of a fixed RateValue (always the sale's: a fixed rule in
	// another currency never matches); empty for percent rates.
	Currency string `json:",omitempty"`

	// Volume-tier provenance (Origin == "volume_tier" only): the crossed
	// threshold and the affiliate's volume at resolution, for the snapshot.
	TierThreshold int64 `json:",omitempty"`
	TierVolume    int64 `json:",omitempty"`
}

// NewEngine binds the engine to a db handle + ledger poster.
func NewEngine(db *sql.DB, poster Poster) *Engine {
	return &Engine{db: db, poster: poster, now: time.Now}
}

// SetClock replaces the engine's clock (created_at, matures_at, the
// maturation cutoff). Replaying history through the real money path — the
// demo seed — needs it. Call before the engine is shared.
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// Process handles one event exactly once. Idempotency: raw_events unique
// (source, external_event_id) — a replay of a FINISHED event returns
// ErrAlreadyProcessed. The raw_events row commits in the same transaction
// as the money it records, so a failed attempt leaves nothing behind and
// the provider's retry starts clean; an unfinished row (processed_at NULL)
// can only come from a pre-atomic release and is re-entered to finish it.
var (
	ErrAlreadyProcessed = fmt.Errorf("commissions: event already processed")
	ErrNoAttribution    = fmt.Errorf("commissions: no attribution for event")
)

// maxCreditAttempts bounds the per-attempt credit idem-key pairs a
// repeatedly failing purchase event may consume (see creditKeys).
const maxCreditAttempts = 20

// credit is what one purchase transaction decided.
type credit struct {
	commissionID, affiliateID, amount int64
	// fresh: this call credited the commission (MLM volume follows);
	// false when it finished an earlier attempt's credit or skipped.
	fresh bool
	// skip is the error a recorded, ACKed skip reports to the caller
	// (ErrNoAttribution, an amount overflow); nil otherwise.
	skip error
}

// ProcessPurchase credits a commission for a purchase/renewal/usage event.
//
// ATOMIC: the raw_events claim, the credit journal, the commission row,
// the customer binding, the daily aggregate, the outbox event and the
// processed marker commit in ONE ledger transaction (Poster.Run). Any
// failure rolls all of it back and the provider's retry starts clean.
func (e *Engine) ProcessPurchase(ctx context.Context, ev Event) (commissionID int64, err error) {
	return e.ProcessPurchaseScreened(ctx, ev, nil)
}

// ScreenFunc screens a credited commission inside the transaction that
// holds it: the verdict and any hold it places go through tx (or a ctx
// derived from the one it is given) and commit with the credit. An error
// rolls the whole credit back.
type ScreenFunc func(ctx context.Context, tx *ledger.Tx, commissionID int64) error

// ProcessPurchaseScreened is ProcessPurchase with screen run inside the
// credit transaction, on the commission it credits or finishes. screen
// may be nil.
func (e *Engine) ProcessPurchaseScreened(ctx context.Context, ev Event, screen ScreenFunc) (commissionID int64, err error) {
	// One spelling per currency ("" = USD, the column default): the
	// commission row, its ledger accounts and its rule match all use it.
	ev.Currency = money.Normalize(ev.Currency)
	// Serialize deliveries of the SAME event in-process: a provider retry
	// racing a slow first attempt waits instead of contending for the
	// transaction. Across replicas the raw_events claim serializes them
	// (the second insert waits on the first's uncommitted row).
	unlock := moneylock.LockIn(ctx, eventLock(ev))
	defer unlock()

	var c credit
	if err := e.poster.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		var err error
		c, err = e.purchaseTx(ctx, tx, ev)
		if err != nil || screen == nil || c.commissionID <= 0 {
			return err
		}
		return screen(ctx, tx, c.commissionID)
	}); err != nil {
		return 0, err
	}
	// MLM business volume: the tree must see every credited dollar. It
	// writes through its own handle, so it runs after the commit (on
	// SQLite a pool write inside the ledger transaction would wait on the
	// write lock that transaction holds). Fail-open; the reconciler heals.
	if c.fresh && e.MLMBV != nil {
		if err := e.MLMBV(ctx, c.affiliateID, dayOf(ev.OccurredAt), ev.Currency, c.amount); err != nil {
			log.Printf("commissions: mlm bv (affiliate=%d amount=%d): %v [reconciler will heal]",
				c.affiliateID, c.amount, err)
		}
	}
	return c.commissionID, c.skip
}

// ScreenCommission runs screen on an existing commission in its own
// ledger transaction. A replayed delivery uses it to finish the screening
// of a commission an earlier release credited and never screened.
func (e *Engine) ScreenCommission(ctx context.Context, commissionID int64, screen ScreenFunc) error {
	unlock := moneylock.LockIn(ctx, moneylock.Commission(commissionID))
	defer unlock()
	return e.poster.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		return screen(ctx, tx, commissionID)
	})
}

// QuotePurchase is the commission ProcessPurchase would credit for ev,
// computed without crediting it: the same affiliate, commission period,
// rule and amount steps, through read-only queries on the pool. It
// claims no event and writes no journal, commission, binding, aggregate
// or outbox row (a Shopify test order's preview, issue 236). 0 means the
// event would earn nothing (unattributed, period ended, no rule in the
// sale's currency, a zero or overflowing amount).
func (e *Engine) QuotePurchase(ctx context.Context, ev Event) (int64, error) {
	ev.Currency = money.Normalize(ev.Currency)
	if ev.AmountMinor <= 0 {
		return 0, nil
	}
	affiliateID, _, campaignID, err := e.resolveAffiliate(ctx, ev)
	if err != nil || affiliateID == 0 {
		return 0, err
	}
	if campaignID > 0 {
		ev.CampaignID = campaignID
	}
	if ended, err := e.periodEnded(ctx, ev, affiliateID); err != nil || ended {
		return 0, err
	}
	if ev.FixedAmount > 0 {
		return ev.FixedAmount, nil
	}
	rule, err := e.resolveRule(ctx, ev, affiliateID)
	if errors.Is(err, ErrCurrencyMismatch) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// An overflow earns nothing, as in purchaseTx (a recorded skip).
	amount, err := computeAmount(rule, ev.AmountMinor)
	if err != nil || amount < 0 {
		return 0, nil
	}
	return amount, nil
}

// purchaseTx is ProcessPurchase's transaction. Configuration reads
// (attribution, rules, program terms) go through the pool: they see
// committed state, which is all they need.
func (e *Engine) purchaseTx(ctx context.Context, tx *ledger.Tx, ev Event) (credit, error) {
	// 1) Ingest idempotency: claim raw_events (finished replay =>
	// ErrAlreadyProcessed).
	if _, err := claimEvent(ctx, tx, ev, ev.EventType); err != nil {
		return credit{}, err
	}

	// 1b) A non-positive gross ($0 trial invoice, 100% coupon, negative
	// adjustment) earns nothing under ANY rate type — a "fixed" rule used
	// to pay its full bounty on a free event. Recorded and ACKed like the
	// other permanent skips; never an error (the webhook must not retry).
	if ev.AmountMinor <= 0 {
		return credit{}, markProcessed(ctx, tx, ev, "non_positive_gross")
	}

	// 2) Resolve affiliate: explicit > coupon > lifetime binding.
	affiliateID, boundMethod, campaignID, err := e.resolveAffiliate(ctx, ev)
	if err != nil {
		return credit{}, err
	}
	if affiliateID == 0 {
		return credit{skip: ErrNoAttribution}, markProcessed(ctx, tx, ev, "unattributed")
	}
	if campaignID > 0 {
		ev.CampaignID = campaignID
	}

	// 2b) Commission period (campaigns.commission_months; 0 = lifetime):
	// a customer earns its affiliate commission for N months, then stops.
	// Recorded and ACKed like the other permanent skips.
	ended, err := e.periodEnded(ctx, ev, affiliateID)
	if err != nil {
		return credit{}, err
	}
	if ended {
		return credit{}, markProcessed(ctx, tx, ev, "commission_period_ended")
	}

	// 3) Resolve rule: coupon override > affiliate override > campaign default.
	// A FixedAmount event (royalty kind) skips resolution: the caller
	// computed the commission (royalty_links), rules do not apply.
	var rule Rule
	var amount int64
	if ev.FixedAmount > 0 {
		rule = Rule{RateType: "fixed", RateValue: ev.FixedAmount, Origin: "caller_fixed"}
		amount = ev.FixedAmount
	} else {
		var err error
		rule, err = e.resolveRule(ctx, ev, affiliateID)
		if errors.Is(err, ErrCurrencyMismatch) {
			// Permanent for this event (rules are not retroactive): recorded
			// and ACKed like the other skips, and logged for the operator.
			log.Printf("commissions: %s:%s earns nothing: %v", ev.Source, ev.ExternalID, err)
			return credit{}, markProcessed(ctx, tx, ev, "currency_mismatch")
		}
		if err != nil {
			return credit{}, err
		}

		// 4) Compute amount (int math only).
		amount, err = computeAmount(rule, ev.AmountMinor)
		if err != nil {
			return credit{skip: err}, markProcessed(ctx, tx, ev, "amount_overflow")
		}
		if amount <= 0 {
			return credit{}, markProcessed(ctx, tx, ev, "zero_commission")
		}
	}

	// 5) Lifetime binding (once per program+customer, immutable).
	if ev.CustomerExtID != "" {
		if err := bindCustomer(ctx, tx, ev.CustomerExtID, affiliateID, ev.CampaignID, boundMethod); err != nil {
			return credit{}, err
		}
	}

	// 6) Ledger: expense -> hold (pending until maturation), under the
	// event's current credit idem-key pair (creditKeys). A journal already
	// under that key is an earlier attempt's, from a release that posted
	// before writing its commission row: adopt it, or — when the rate
	// changed since — retire it (its revert spends the pair) and credit
	// under the next pair, in this same transaction.
	var journalID int64
	for {
		idem, revertKey, err := creditKeys(ctx, tx, ev)
		if err != nil {
			return credit{}, err
		}
		journalID, err = e.poster.Post(ctx, idem, "conversion", ev.ExternalID, ev.OccurredAt, []ledger.Line{
			{Account: ledger.Expense(ev.Currency), Amount: amount},
			{Account: ledger.Hold(ev.Currency), Amount: -amount},
		})
		if err == nil {
			break
		}
		if !errors.Is(err, ledger.ErrDuplicate) {
			return credit{}, fmt.Errorf("commissions: ledger post: %w", err)
		}
		var existing int64
		qerr := tx.QueryRowContext(ctx,
			`SELECT id FROM commissions WHERE journal_id = ?`, journalID).Scan(&existing)
		switch {
		case qerr == nil:
			// Only the processed marker was lost.
			return credit{commissionID: existing, affiliateID: affiliateID, amount: amount},
				markProcessedOK(ctx, tx, ev)
		case !errors.Is(qerr, sql.ErrNoRows):
			return credit{}, fmt.Errorf("commissions: adopt lookup: %w", qerr)
		}
		posted, err := journalAmount(ctx, tx, journalID, ledger.Expense(ev.Currency))
		if err != nil {
			return credit{}, err
		}
		if posted == amount {
			break // adopt the earlier attempt's journal
		}
		if _, err := e.poster.Post(ctx, revertKey, "purchase_revert", ev.ExternalID, e.now().UTC(), []ledger.Line{
			{Account: ledger.Expense(ev.Currency), Amount: -posted},
			{Account: ledger.Hold(ev.Currency), Amount: posted},
		}); err != nil {
			return credit{}, fmt.Errorf("commissions: retire credit journal %d (holds %d, recomputed %d): %w",
				journalID, posted, amount, err)
		}
	}

	// 7) Commission row (pending, matures by the PROGRAM's NET terms;
	// program_settings.default_hold_days when the program row is
	// unreadable). net_terms_days = 0 means no hold.
	snap, _ := json.Marshal(rule)
	floor := 0
	if e.HoldFloor != nil {
		floor = e.HoldFloor(ctx)
	}
	netDays := settings.HoldDaysAtLeast(ctx, e.db, ev.CampaignID, floor)
	matures := e.now().UTC().Add(time.Duration(netDays) * 24 * time.Hour)
	c := credit{affiliateID: affiliateID, amount: amount, fresh: true}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency,
		                         rate_snapshot, status, matures_at, created_at, buyer_email,
		                         attribution_method, coupon_code, customer_ext_id, country,
		                         gross_minor, click_id, order_id)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		affiliateID, ev.CampaignID, journalID, amount, ev.Currency, string(snap),
		matures.Format(time.RFC3339Nano), e.now().UTC().Format(time.RFC3339Nano),
		strings.ToLower(strings.TrimSpace(ev.CustomerEmail)),
		boundMethod, ev.CouponCode, ev.CustomerExtID, ev.Country, ev.AmountMinor,
		nullableStr(ev.ClickID), nullableStr(ev.OrderID)).Scan(&c.commissionID); err != nil {
		return credit{}, fmt.Errorf("commissions: insert: %w", err)
	}

	// 8) Aggregate update (on-write, batch-safe upsert).
	if err := bumpDaily(ctx, tx, ev, affiliateID, amount); err != nil {
		return credit{}, err
	}

	// 9) Outbox event (same transaction; at-least-once downstream).
	if err := emitOutbox(ctx, tx, "commission.created", map[string]any{
		"commission_id": c.commissionID, "affiliate_id": affiliateID,
		"amount": amount, "currency": ev.Currency, "source": ev.Source,
		"external_id": ev.ExternalID, "method": boundMethod,
	}); err != nil {
		return credit{}, err
	}
	return c, markProcessedOK(ctx, tx, ev)
}

// clawParent is the parent commission a refund claws against.
type clawParent struct {
	id, amount, affiliateID, campaignID int64
	status, currency                    string
}

// ProcessRefund claws back proportionally. ratio = refundedMinor / originalMinor,
// where refundedMinor is THIS refund's amount (not a cumulative total).
//
// Refunds accumulate per parent: each clawback row records the refund
// amount it covered (gross_minor), so a partial refund claws exactly the
// increment amount x cumulative/original minus what earlier refunds already
// clawed — rounding-exact, never past the parent. The parent moves to its
// terminal status only when a refund exhausts it (cumulative full refund);
// until then it keeps its state and later partial refunds claw their share.
func (e *Engine) ProcessRefund(ctx context.Context, ev Event, originalCommissionID int64, refundedMinor, originalMinor int64) error {
	return e.processRefund(ctx, ev, originalCommissionID, refundedMinor, originalMinor, false)
}

// ProcessRefundCumulative is ProcessRefund for providers that report the
// CUMULATIVE refunded total on every refund event (Lemon Squeezy's
// attributes.refunded) instead of the per-refund amount: the engine claws
// only the part of that running total not already clawed, so a second
// partial refund never re-claws the first one's share.
func (e *Engine) ProcessRefundCumulative(ctx context.Context, ev Event, originalCommissionID int64, cumulativeRefundedMinor, originalMinor int64) error {
	return e.processRefund(ctx, ev, originalCommissionID, cumulativeRefundedMinor, originalMinor, true)
}

// processRefund runs the refund in ONE ledger transaction: raw_events
// claim, parent transition, claw journal, clawback row and processed
// marker commit together. The parent row is locked first (FOR UPDATE on
// Postgres), so a maturation, uphold, payout settle or another refund of
// the same commission — in this process or any other replica — waits for
// this transaction and then sees its result.
func (e *Engine) processRefund(ctx context.Context, ev Event, originalCommissionID int64, refundedMinor, originalMinor int64, cumulativeMode bool) error {
	// In-process fast path: queue same-commission rivals here instead of
	// on the conn mutex. Every delivery of one refund event targets the
	// same parent, so this also serializes provider retries of the event.
	unlock := moneylock.LockIn(ctx, moneylock.Commission(originalCommissionID))
	defer unlock()
	return e.poster.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		return e.refundTx(ctx, tx, ev, originalCommissionID, refundedMinor, originalMinor, cumulativeMode)
	})
}

func (e *Engine) refundTx(ctx context.Context, tx *ledger.Tx, ev Event, originalCommissionID int64, refundedMinor, originalMinor int64, cumulativeMode bool) error {
	reentered, err := claimEvent(ctx, tx, ev, "refund")
	if err != nil {
		return err
	}
	idem := fmt.Sprintf("clawback:%s:%s", ev.Source, ev.ExternalID)

	p := clawParent{id: originalCommissionID}
	if err := tx.QueryRowContext(ctx, `
		SELECT amount, affiliate_id, campaign_id, status, currency
		FROM commissions WHERE id = ?`+tx.ForUpdate(),
		originalCommissionID).Scan(&p.amount, &p.affiliateID, &p.campaignID, &p.status, &p.currency); err != nil {
		return fmt.Errorf("commissions: parent lookup: %w", err)
	}
	refundedSoFar, clawed, err := refundTotals(ctx, tx, p.id)
	if err != nil {
		return err
	}
	remaining := p.amount - clawed

	// Exact integer proration (attack 9 family: no float in money math).
	if originalMinor <= 0 {
		originalMinor = 1
	}
	if refundedMinor < 0 {
		refundedMinor = 0
	}
	if refundedMinor > originalMinor {
		refundedMinor = originalMinor // clamp at full refund
	}
	var cumulative int64
	if cumulativeMode {
		// The provider reports the running total: never below what is
		// already clawed (an out-of-order older delivery claws nothing).
		cumulative = refundedMinor
		if cumulative < refundedSoFar {
			cumulative = refundedSoFar
		}
	} else {
		cumulative = originalMinor // clamp the running total at full refund
		if refundedSoFar < originalMinor-refundedMinor {
			cumulative = refundedSoFar + refundedMinor
		}
	}
	refundInc := cumulative - refundedSoFar
	if refundInc < 0 {
		refundInc = 0
	}
	target, ok := bpmath.MulDiv(p.amount, cumulative, originalMinor)
	if !ok {
		return fmt.Errorf("commissions: clawback overflow: amount=%d refunded=%d original=%d",
			p.amount, cumulative, originalMinor)
	}
	claw := target - clawed
	if claw > remaining {
		claw = remaining // never claw past the parent
	}

	if reentered {
		// A pre-atomic attempt of this refund posted its claw journal and
		// died before the clawback row: finish the row from the journal.
		jid, err := journalByKey(ctx, tx, idem)
		if err != nil {
			return err
		}
		if jid != 0 {
			return e.finishClawback(ctx, tx, ev, p, jid, refundInc)
		}
	}

	// The claw account follows the parent's state, read under the parent's
	// row lock (maturation, uphold and payout settle all take it).
	// pending: money is in HOLD. approved: in payable. paid: in payable
	// (post-payout reversal — PostClawback may take it positive: the
	// affiliate owes us, netted by the next payout batch).
	var fromAccount, terminal string
	switch p.status {
	case "pending":
		fromAccount, terminal = ledger.Hold(p.currency), "cancelled"
	case "approved":
		fromAccount, terminal = ledger.Payable(p.affiliateID, p.currency), "cancelled"
	case "paid":
		fromAccount, terminal = ledger.Payable(p.affiliateID, p.currency), "clawback"
	default:
		// cancelled / clawback: a fraud uphold or an earlier full refund
		// already took the money back — unless a pre-atomic attempt of
		// THIS refund moved the parent and died before its claw posted.
		if reentered && remaining > 0 {
			return e.resumeTerminalClawback(ctx, tx, ev, p, idem, remaining, refundInc)
		}
		return markProcessed(ctx, tx, ev, "parent_terminal")
	}
	if remaining <= 0 {
		return markProcessed(ctx, tx, ev, "parent_terminal")
	}
	if claw <= 0 {
		return markProcessed(ctx, tx, ev, "zero_clawback")
	}

	// Only the refund that exhausts the parent moves it to its terminal
	// status. The row lock makes the read stable; the guard stays as
	// defence.
	if claw == remaining {
		res, err := tx.ExecContext(ctx, `
			UPDATE commissions SET status = ? WHERE id = ? AND status = ?`,
			terminal, p.id, p.status)
		if err != nil {
			return fmt.Errorf("commissions: parent transition: %w", err)
		}
		if c, _ := res.RowsAffected(); c != 1 {
			return fmt.Errorf("commissions: parent %d left %s under its row lock", p.id, p.status)
		}
	}
	clawJournalID, err := e.postClawback(ctx, idem, "clawback", ev.ExternalID, ev.OccurredAt, []ledger.Line{
		{Account: fromAccount, Amount: claw},
		{Account: ledger.Expense(p.currency), Amount: -claw},
	})
	if err != nil && !errors.Is(err, ledger.ErrDuplicate) {
		return fmt.Errorf("commissions: clawback post: %w", err)
	}
	if err := e.insertClawbackRow(ctx, tx, p, clawJournalID, claw, refundInc, ev.Reason); err != nil {
		return err
	}
	return markProcessedOK(ctx, tx, ev)
}

// finishClawback completes a refund whose claw journal landed on an
// earlier (pre-atomic) attempt: the clawback row is written from the
// journal's amount (unless it exists already) and the event is marked
// processed.
func (e *Engine) finishClawback(ctx context.Context, tx *ledger.Tx, ev Event, p clawParent, journalID, refundInc int64) error {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM commissions WHERE journal_id = ?`, journalID).Scan(&n); err != nil {
		return fmt.Errorf("commissions: clawback row lookup: %w", err)
	}
	if n == 0 {
		expense, err := journalAmount(ctx, tx, journalID, ledger.Expense(p.currency))
		if err != nil {
			return err
		}
		if err := e.insertClawbackRow(ctx, tx, p, journalID, -expense, refundInc, ev.Reason); err != nil {
			return err
		}
	}
	return markProcessedOK(ctx, tx, ev)
}

// resumeTerminalClawback finishes a full refund whose status CAS landed but
// whose claw journal never posted (a pre-atomic release). The money still
// sits where the pre-refund state kept it: payable once matured (a
// mature:<id> journal exists, or the parent reached 'clawback' from paid),
// HOLD otherwise. A fraud uphold (fraudclaw:<id>) means the money was
// already clawed.
func (e *Engine) resumeTerminalClawback(ctx context.Context, tx *ledger.Tx, ev Event, p clawParent, idem string, remaining, refundInc int64) error {
	upheld, err := journalByKey(ctx, tx, fmt.Sprintf("fraudclaw:%d", p.id))
	if err != nil {
		return err
	}
	if upheld != 0 {
		return markProcessed(ctx, tx, ev, "parent_terminal")
	}
	fromAccount := ledger.Payable(p.affiliateID, p.currency)
	if p.status == "cancelled" {
		matured, err := journalByKey(ctx, tx, fmt.Sprintf("mature:%d", p.id))
		if err != nil {
			return err
		}
		if matured == 0 {
			fromAccount = ledger.Hold(p.currency)
		}
	}
	jid, err := e.postClawback(ctx, idem, "clawback", ev.ExternalID, ev.OccurredAt, []ledger.Line{
		{Account: fromAccount, Amount: remaining},
		{Account: ledger.Expense(p.currency), Amount: -remaining},
	})
	if err != nil && !errors.Is(err, ledger.ErrDuplicate) {
		return fmt.Errorf("commissions: resumed clawback post: %w", err)
	}
	if err := e.insertClawbackRow(ctx, tx, p, jid, remaining, refundInc, ev.Reason); err != nil {
		return err
	}
	return markProcessedOK(ctx, tx, ev)
}

// insertClawbackRow records a clawback (negative amount) linked to its
// parent, in the parent's currency and campaign so payout netting groups
// it with the money it reverses. gross_minor records the refund amount the
// row covered (the per-parent cumulative refund basis); clawback rows never
// count as sales volume (tier volume sums amount > 0 rows only).
func (e *Engine) insertClawbackRow(ctx context.Context, tx *ledger.Tx, p clawParent, journalID, claw, refundInc int64, reason string) error {
	snap := `{"origin":"clawback"}`
	if reason != "" {
		snap = string(mustJSON(map[string]string{"origin": "clawback", "reason": reason}))
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO commissions (affiliate_id, campaign_id, journal_id, amount, currency,
		                         rate_snapshot, status, parent_commission_id, created_at, gross_minor)
		VALUES (?, ?, ?, ?, ?, ?, 'cancelled', ?, ?, ?)`,
		p.affiliateID, p.campaignID, journalID, -claw, p.currency, snap, p.id,
		e.now().UTC().Format(time.RFC3339Nano), refundInc); err != nil {
		return fmt.Errorf("commissions: clawback row: %w", err)
	}
	return nil
}

// RefundedBasis returns the refund amount (in the parent's gross
// basis) already covered by a commission's clawback rows: the
// cumulative a ProcessRefundCumulative caller has applied so far. q is
// the pool or the open money transaction (to see its own writes).
func RefundedBasis(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, commissionID int64,
) (int64, error) {
	var refunded int64
	if err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(gross_minor), 0) FROM commissions
		WHERE parent_commission_id = ? AND amount < 0`, commissionID).Scan(&refunded); err != nil {
		return 0, fmt.Errorf("commissions: refunded basis: %w", err)
	}
	return refunded, nil
}

// refundTotals returns, for a parent commission, the refund amount already
// covered by its clawback rows and the commission already clawed (both >= 0).
func refundTotals(ctx context.Context, q dbtx, parentID int64) (refunded, clawed int64, err error) {
	var sum int64
	if err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(gross_minor), 0), COALESCE(SUM(amount), 0) FROM commissions
		WHERE parent_commission_id = ? AND amount < 0`, parentID).Scan(&refunded, &sum); err != nil {
		return 0, 0, fmt.Errorf("commissions: refund totals: %w", err)
	}
	return refunded, -sum, nil
}

// Mature approves pending commissions whose matures_at has passed.
// Exactly-once via status transition guard.
//
// ATOMIC: per commission, the HOLD -> payable journal and the
// pending -> approved transition commit in ONE ledger transaction, with
// the commission row locked (FOR UPDATE on Postgres) before its state and
// refund totals are read. A refund or uphold of the same commission in any
// replica either finished first (the row is no longer pending, or its
// clawbacks are counted) or waits for this one.
//
// matures_at is written RFC3339Nano (see ProcessPurchase); the comparison
// must bind the SAME format: the SQLite driver renders time.Time params as
// Go's t.String() ("2006-01-02 15:04:05 +0000 UTC"), whose space separator
// sorts before 'T' — same-day rows would misorder and mature up to 24h late.
func (e *Engine) Mature(ctx context.Context) (n int64, err error) {
	now := e.now().UTC()
	rows, err := e.db.QueryContext(ctx, `
		SELECT id FROM commissions
		WHERE status = 'pending' AND matures_at <= ? LIMIT 100`,
		now.Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("commissions: mature query: %w", err)
	}
	var batch []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		batch = append(batch, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("commissions: mature scan: %w", err)
	}

	for _, id := range batch {
		won, err := e.matureOne(ctx, id, now)
		if err != nil {
			return n, fmt.Errorf("commissions: mature %d: %w", id, err)
		}
		if won {
			n++
		}
	}
	return n, nil
}

// matureOne matures one commission in its own transaction. won=false: a
// rival moved it out of 'pending' first; nothing moves.
func (e *Engine) matureOne(ctx context.Context, id int64, now time.Time) (won bool, err error) {
	unlock := moneylock.LockIn(ctx, moneylock.Commission(id))
	defer unlock()
	err = e.poster.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		won = false
		var aff, amount int64
		var status, cur string
		if err := tx.QueryRowContext(ctx, `
			SELECT affiliate_id, amount, status, currency FROM commissions WHERE id = ?`+tx.ForUpdate(),
			id).Scan(&aff, &amount, &status, &cur); err != nil {
			return fmt.Errorf("lookup: %w", err)
		}
		if status != "pending" {
			return nil
		}
		// Partial refunds clawed part of this commission straight out of
		// HOLD while it was pending: only the unclawed remainder moves to
		// payable.
		_, clawed, err := refundTotals(ctx, tx, id)
		if err != nil {
			return err
		}
		// ErrDuplicate: a pre-atomic attempt's journal landed and its CAS
		// never did. The money is moved; finish with the CAS.
		if net := amount - clawed; net > 0 {
			if _, err := e.poster.Post(ctx, fmt.Sprintf("mature:%d", id), "maturation", "", now, []ledger.Line{
				{Account: ledger.Hold(cur), Amount: net},
				{Account: ledger.Payable(aff, cur), Amount: -net},
			}); err != nil && !errors.Is(err, ledger.ErrDuplicate) {
				return fmt.Errorf("post: %w", err)
			}
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE commissions SET status='approved', approved_at=?
			WHERE id = ? AND status = 'pending'`,
			now.Format(time.RFC3339Nano), id)
		if err != nil {
			return fmt.Errorf("status: %w", err)
		}
		if c, _ := res.RowsAffected(); c != 1 {
			return fmt.Errorf("status: commission left pending under its row lock")
		}
		won = true
		return nil
	})
	return won, err
}

// --- internals ---

// dbtx is the read/write surface shared by the pool and a ledger.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// claimEvent inserts the raw_events idempotency row inside the money
// transaction. A finished event (processed_at set) replays as
// ErrAlreadyProcessed. A row left unfinished (processed_at NULL) by a
// pre-atomic release is RE-ENTERED (reentered=true) so the retry can
// complete what that attempt started. ON CONFLICT DO NOTHING keeps the
// claim statement-safe on Postgres, where a failed INSERT would abort the
// whole transaction; a concurrent claim of the same event in another
// replica waits for this transaction and then finds the row.
func claimEvent(ctx context.Context, q dbtx, ev Event, eventType string) (reentered bool, err error) {
	res, err := q.ExecContext(ctx, `
		INSERT INTO raw_events (source, external_event_id, event_type, payload)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(source, external_event_id) DO NOTHING`,
		ev.Source, ev.ExternalID, eventType, mustJSON(ev))
	if err != nil {
		return false, fmt.Errorf("commissions: raw_events insert: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, fmt.Errorf("commissions: raw_events insert: %w", err)
	} else if n == 1 {
		return false, nil
	}
	var unfinished int
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM raw_events
		WHERE source = ? AND external_event_id = ? AND processed_at IS NULL`,
		ev.Source, ev.ExternalID).Scan(&unfinished); err != nil {
		return false, fmt.Errorf("commissions: raw_events lookup: %w", err)
	}
	if unfinished == 0 {
		return false, ErrAlreadyProcessed
	}
	return true, nil
}

// eventLock names the moneylock serializing deliveries of one event.
func eventLock(ev Event) string {
	return "event:" + ev.Source + ":" + ev.ExternalID
}

// creditKeys returns the credit/revert idem-key pair for this attempt of a
// purchase. Attempt 0 keeps the historical keys; an attempt whose credit
// was reverted (its purchase-revert journal exists) is spent, so the next
// attempt moves to the next suffixed pair. Spent pairs come from releases
// that posted the credit outside the commission's transaction; an atomic
// attempt that fails leaves no journal and keeps its pair.
func creditKeys(ctx context.Context, q dbtx, ev Event) (credit, revert string, err error) {
	base := ev.Source + ":" + ev.ExternalID
	for k := 0; k < maxCreditAttempts; k++ {
		suffix := ""
		if k > 0 {
			suffix = fmt.Sprintf("#%d", k)
		}
		credit, revert = "commission:"+base+suffix, "purchase-revert:"+base+suffix
		rid, err := journalByKey(ctx, q, revert)
		if err != nil {
			return "", "", err
		}
		if rid == 0 {
			return credit, revert, nil
		}
	}
	return "", "", fmt.Errorf("commissions: event %s spent %d credit attempts; operator attention required",
		base, maxCreditAttempts)
}

// CreditedCommission returns the live commission a purchase event
// credited (0 = none: the event was unattributed, skipped, or never
// credited). Billing adapters call it on a replayed delivery to finish
// the refund linkage an earlier attempt could not write. Credit pairs are
// consumed in order (creditKeys), so the walk stops at the first unused one.
func (e *Engine) CreditedCommission(ctx context.Context, source, externalID string) (int64, error) {
	base := source + ":" + externalID
	for k := 0; k < maxCreditAttempts; k++ {
		key := "commission:" + base
		if k > 0 {
			key += fmt.Sprintf("#%d", k)
		}
		jid, err := journalByKey(ctx, e.db, key)
		if err != nil || jid == 0 {
			return 0, err
		}
		var id int64
		err = e.db.QueryRowContext(ctx, `SELECT id FROM commissions WHERE journal_id = ?`, jid).Scan(&id)
		switch {
		case err == nil:
			return id, nil
		case !errors.Is(err, sql.ErrNoRows):
			return 0, fmt.Errorf("commissions: credited lookup: %w", err)
		}
	}
	return 0, nil
}

// CreditedCommissionIn is CreditedCommission read through q — the open
// money transaction, so it sees a credit that transaction made — and
// also returns the commission's gross basis (the event's AmountMinor).
func CreditedCommissionIn(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, source, externalID string,
) (commissionID, grossMinor int64, err error) {
	base := source + ":" + externalID
	for k := 0; k < maxCreditAttempts; k++ {
		key := "commission:" + base
		if k > 0 {
			key += fmt.Sprintf("#%d", k)
		}
		var id, gross int64
		err := q.QueryRowContext(ctx, `
			SELECT c.id, c.gross_minor FROM journal_entries j
			JOIN commissions c ON c.journal_id = j.id
			WHERE j.idem_key = ?`, key).Scan(&id, &gross)
		switch {
		case err == nil:
			return id, gross, nil
		case !errors.Is(err, sql.ErrNoRows):
			return 0, 0, fmt.Errorf("commissions: credited lookup: %w", err)
		}
		// No commission on this pair: walk on only past a spent pair
		// (its journal exists; creditKeys moved to the next).
		var jid int64
		err = q.QueryRowContext(ctx, `SELECT id FROM journal_entries WHERE idem_key = ?`, key).Scan(&jid)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		if err != nil {
			return 0, 0, fmt.Errorf("commissions: credited lookup: %w", err)
		}
	}
	return 0, 0, nil
}

// journalByKey returns the journal id posted under idemKey (0 = none).
func journalByKey(ctx context.Context, q dbtx, idemKey string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM journal_entries WHERE idem_key = ?`, idemKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("commissions: journal lookup %q: %w", idemKey, err)
	}
	return id, nil
}

// journalAmount returns the signed amount a journal posted to one account.
func journalAmount(ctx context.Context, q dbtx, journalID int64, account string) (int64, error) {
	var v int64
	if err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(le.amount), 0)
		FROM ledger_entries le
		JOIN ledger_accounts la ON la.id = le.account_id
		WHERE le.journal_id = ? AND la.code = ?`, journalID, account).Scan(&v); err != nil {
		return 0, fmt.Errorf("commissions: journal %d amount: %w", journalID, err)
	}
	return v, nil
}

// postClawback posts a refund clawback through the ledger's clawback entry
// point when the poster has one (the payable may go positive: the
// affiliate owes us, netted by the next payout batch), else through Post.
// Called with a Run ctx, it posts inside that transaction.
func (e *Engine) postClawback(ctx context.Context, idemKey, source, description string, effective time.Time, lines []ledger.Line) (int64, error) {
	if cp, ok := e.poster.(clawbackPoster); ok {
		return cp.PostClawback(ctx, idemKey, source, description, effective, lines)
	}
	return e.poster.Post(ctx, idemKey, source, description, effective, lines)
}

// resolveAffiliate returns the affiliate an event credits, its method, and
// the program a fallback rung resolved (0 = keep ev.CampaignID).
//
// The coupon/customer fallback mirrors the attribution ladder: only an
// ACTIVE affiliate earns, and a rung that matches a frozen or removed
// affiliate ends resolution as unattributed rather than falling through.
// A customer binding credits in the program it was bound under;
// customers.program_id is the binding namespace, always 1 (BindCustomer).
func (e *Engine) resolveAffiliate(ctx context.Context, ev Event) (int64, string, int64, error) {
	if ev.AffiliateID != 0 {
		switch ev.Method {
		// The Shopify ladder's rungs (pixel_join, pos, journey,
		// ip_fallback) and a merchant's manual assignment keep their name
		// as provenance.
		case "token", "coupon", "customer", "pixel_join", "pos", "journey", "ip_fallback", "manual":
			return ev.AffiliateID, ev.Method, 0, nil
		}
		return ev.AffiliateID, "explicit", 0, nil
	}
	if ev.CouponCode != "" {
		var aff, camp int64
		var status string
		err := e.db.QueryRowContext(ctx, `
			SELECT c.affiliate_id, c.campaign_id, a.status FROM coupons c
			JOIN affiliates a ON a.id = c.affiliate_id
			WHERE c.code = ? AND c.active = TRUE`, ev.CouponCode).Scan(&aff, &camp, &status)
		if err == nil {
			if status != "active" {
				return 0, "", 0, nil
			}
			return aff, "coupon", camp, nil
		}
		if err != sql.ErrNoRows {
			return 0, "", 0, fmt.Errorf("commissions: coupon lookup: %w", err)
		}
	}
	if ev.CustomerExtID != "" {
		var aff, camp int64
		var status string
		err := e.db.QueryRowContext(ctx, `
			SELECT cu.affiliate_id, cu.campaign_id, a.status FROM customers cu
			JOIN affiliates a ON a.id = cu.affiliate_id
			WHERE cu.program_id = ? AND cu.external_customer_id = ?`, CustomerNamespace, ev.CustomerExtID).Scan(&aff, &camp, &status)
		if err == nil {
			if status != "active" {
				return 0, "", 0, nil
			}
			return aff, "customer", camp, nil
		}
		if err != sql.ErrNoRows {
			return 0, "", 0, fmt.Errorf("commissions: customer lookup: %w", err)
		}
	}
	// Token resolution happens in the httpapi layer (parses the signed token
	// and sets AffiliateID); reaching here without it = unattributed.
	return 0, "", 0, nil
}

// ErrCurrencyMismatch: the sale's currency matches no rule. Every rule
// that would apply is a fixed amount in another currency (a "fixed 500"
// set in USD must not pay ¥500 on a yen sale), so the sale earns nothing;
// it is recorded and ACKed as "currency_mismatch".
var ErrCurrencyMismatch = errors.New("commissions: no rule in the sale's currency")

// ErrNoRule: the program has no active commission rule at all (a shop
// that never saved a rate). Unlike ErrCurrencyMismatch it is not a
// verdict on the event: the sale earns once a rule exists, so the event
// is not claimed (the transaction rolls back) and the caller decides
// whether to wait for a rule (the Shopify fold does) or to retry.
var ErrNoRule = errors.New("commissions: no rule")

// ErrAmountOverflow is a commission that overflows int64 minor units:
// recorded and ACKed as a skip (nothing credited).
var ErrAmountOverflow = errors.New("commissions: amount overflow")

// fixedMatches is the rule-currency predicate: a percent rule applies to
// every currency; a fixed rule only to sales in its own currency (NULL =
// the program currency). Binds (program currency, sale currency).
const fixedMatches = `(rate_type <> 'fixed' OR COALESCE(currency, ?) = ?)`

// resolveRule picks the rule a sale earns under. Fixed amounts are money
// in one currency: a fixed rule (or coupon) in another currency than the
// sale's is skipped and resolution continues down the ladder, so a yen
// sale past a "fixed $5" affiliate override earns the campaign's percent
// default. Volume tiers are measured and matched per currency (no FX).
func (e *Engine) resolveRule(ctx context.Context, ev Event, affiliateID int64) (Rule, error) {
	progCur := settings.Currency(ctx, e.db)
	cur := ev.Currency
	// Coupon override first.
	if ev.CouponCode != "" {
		var rt string
		var rv int64
		var cc sql.NullString
		err := e.db.QueryRowContext(ctx, `
			SELECT rate_type, rate_value, currency FROM coupons WHERE code = ? AND active = TRUE`,
			ev.CouponCode).Scan(&rt, &rv, &cc)
		if err == nil && rt != "inherit" && (rt != "fixed" || ruleCurrency(cc, progCur) == cur) {
			return Rule{RateType: rt, RateValue: rv, Origin: "coupon", Currency: ruleCurrencyIfFixed(rt, cc, progCur)}, nil
		}
	}
	// Rule tiers (product-aware, FR-12): within both the affiliate-override
	// and campaign-default tiers, a rule whose product_key equals the
	// event's ProductKey outranks the product-agnostic fallback (NULL).
	// Priority breaks ties inside a tier; coupon stays the top override.
	// (event_type intentionally NOT filtered here — pre-existing semantics:
	// priority-ordered match across active rules; renewal/reusage events
	// have always shared the purchase rule set.)
	key := ev.ProductKey
	lookup := func(origin, where string, args ...any) (Rule, bool, error) {
		var r Rule
		var cc sql.NullString
		args = append(args, progCur, cur)
		err := e.db.QueryRowContext(ctx, `
			SELECT rate_type, rate_value, currency FROM commission_rules
			WHERE `+where+` AND active = TRUE AND `+fixedMatches+`
			ORDER BY priority DESC LIMIT 1`, args...).Scan(&r.RateType, &r.RateValue, &cc)
		if errors.Is(err, sql.ErrNoRows) {
			return Rule{}, false, nil
		}
		if err != nil {
			return Rule{}, false, fmt.Errorf("commissions: rule lookup: %w", err)
		}
		r.Origin, r.Currency = origin, ruleCurrencyIfFixed(r.RateType, cc, progCur)
		return r, true, nil
	}
	// Affiliate tier: product-specific first, then product-agnostic.
	if key != "" {
		if r, ok, err := lookup("affiliate_product",
			`campaign_id = ? AND affiliate_id = ? AND product_key = ?`, ev.CampaignID, affiliateID, key); err != nil || ok {
			return r, err
		}
	}
	if r, ok, err := lookup("affiliate_override",
		`campaign_id = ? AND affiliate_id = ? AND product_key IS NULL`, ev.CampaignID, affiliateID); err != nil || ok {
		return r, err
	}
	// Campaign tier: product-specific first, then product-agnostic.
	if key != "" {
		if r, ok, err := lookup("campaign_product",
			`campaign_id = ? AND affiliate_id IS NULL AND product_key = ?`, ev.CampaignID, key); err != nil || ok {
			return r, err
		}
	}
	base, ok, err := lookup("campaign_default",
		`campaign_id = ? AND affiliate_id IS NULL AND product_key IS NULL`, ev.CampaignID)
	if err != nil {
		return Rule{}, err
	}
	if !ok {
		// No rule at all, or only fixed rules in other currencies (the
		// rules the ladder above would have picked but for the currency).
		var n int
		if err := e.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM commission_rules
			WHERE campaign_id = ? AND active = TRUE AND rate_type = 'fixed'
			  AND (affiliate_id IS NULL OR affiliate_id = ?)
			  AND (product_key IS NULL OR product_key = ?)`, ev.CampaignID, affiliateID, key).Scan(&n); err != nil {
			return Rule{}, fmt.Errorf("commissions: default rule: %w", err)
		}
		if n > 0 {
			return Rule{}, fmt.Errorf("%w: %s sale, campaign %d", ErrCurrencyMismatch, cur, ev.CampaignID)
		}
		return Rule{}, fmt.Errorf("%w for campaign %d", ErrNoRule, ev.CampaignID)
	}

	// Volume tiers (FR-12): a crossed tier replaces the BASE campaign rate.
	// Tiers never outrank affiliate/coupon overrides (returned above) nor
	// campaign+product economics (returned above) — they are the volume
	// knob on the default rate only. Basis: the affiliate's cumulative
	// referred revenue (gross) in this campaign IN THE SALE'S CURRENCY,
	// EXCLUDING the current event (measured before this commission lands —
	// crossing unlocks the rate on subsequent sales; TestTiers_ThresholdCross
	// pins this), and excluding clawbacks. Only tiers in the sale's
	// currency apply: yen volume never crosses a dollar threshold. A
	// threshold-0 tier fires from an affiliate's very first sale in its
	// currency ("newcomer rate").
	var volume int64
	if err := e.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(gross_minor), 0) FROM commissions
		WHERE affiliate_id = ? AND campaign_id = ? AND currency = ? AND amount > 0
		  AND status NOT IN ('cancelled','clawback')`, affiliateID, ev.CampaignID, cur).Scan(&volume); err != nil {
		return Rule{}, fmt.Errorf("commissions: volume basis: %w", err)
	}
	if tier, ok := e.crossedTier(ctx, ev, progCur, volume); ok {
		tier.TierVolume = volume
		return tier, nil
	}
	return base, nil
}

// ruleCurrency is the currency a rule's money is in: its own, or the
// program currency when it names none.
func ruleCurrency(cc sql.NullString, progCur string) string {
	if cc.Valid && cc.String != "" {
		return money.Normalize(cc.String)
	}
	return progCur
}

// ruleCurrencyIfFixed is the snapshot's currency: set for a fixed amount
// (it is money), empty for a percent rate (it is not).
func ruleCurrencyIfFixed(rt string, cc sql.NullString, progCur string) string {
	if rt != "fixed" {
		return ""
	}
	return ruleCurrency(cc, progCur)
}

// crossedTier resolves the highest crossed volume tier for the event, in
// the event's currency: a product-specific tier outranks the
// product-agnostic ladder; within each, the highest threshold <= volume
// wins. Lookup errors surface as "no tier crossed" (fail-open to the base
// rate — a tiering glitch must never block a conversion; the base rate
// still credits correctly).
func (e *Engine) crossedTier(ctx context.Context, ev Event, progCur string, volume int64) (Rule, bool) {
	const qSpecific = `
		SELECT rate_type, rate_value, threshold_minor FROM rate_tiers
		WHERE campaign_id = ? AND active = TRUE AND product_key = ?
		  AND COALESCE(currency, ?) = ? AND threshold_minor <= ?
		ORDER BY threshold_minor DESC LIMIT 1`
	const qAny = `
		SELECT rate_type, rate_value, threshold_minor FROM rate_tiers
		WHERE campaign_id = ? AND active = TRUE AND product_key IS NULL
		  AND COALESCE(currency, ?) = ? AND threshold_minor <= ?
		ORDER BY threshold_minor DESC LIMIT 1`
	var rt string
	var rv, threshold int64
	var err error
	if ev.ProductKey != "" {
		err = e.db.QueryRowContext(ctx, qSpecific, ev.CampaignID, ev.ProductKey, progCur, ev.Currency, volume).Scan(&rt, &rv, &threshold)
	} else {
		err = e.db.QueryRowContext(ctx, qAny, ev.CampaignID, progCur, ev.Currency, volume).Scan(&rt, &rv, &threshold)
	}
	tier := func() Rule {
		r := Rule{RateType: rt, RateValue: rv, Origin: "volume_tier", TierThreshold: threshold, Currency: ev.Currency}
		if rt != "fixed" {
			r.Currency = ""
		}
		return r
	}
	if err == nil {
		return tier(), true
	}
	if err != sql.ErrNoRows || ev.ProductKey == "" {
		return Rule{}, false // real lookup error, or no agnostic ladder left to try
	}
	// No product-specific tier crossed: try the product-agnostic ladder.
	err = e.db.QueryRowContext(ctx, qAny, ev.CampaignID, progCur, ev.Currency, volume).Scan(&rt, &rv, &threshold)
	if err == nil {
		return tier(), true
	}
	return Rule{}, false
}

// computeAmount applies the rule's rate. Percent math routes through
// bpmath (128-bit intermediates): a near-2^63 gross must be REJECTED, not
// wrapped or silently dropped via the zero-result path (attack 9 pin). A
// non-positive gross earns nothing under any rate type ($0 trials must
// never pay a fixed bounty).
func computeAmount(r Rule, gross int64) (int64, error) {
	if gross <= 0 {
		return 0, nil
	}
	switch r.RateType {
	case "percent":
		v, ok := bpmath.MulDivBP(gross, r.RateValue)
		if !ok {
			return 0, fmt.Errorf("%w: gross=%d rate_bp=%d", ErrAmountOverflow, gross, r.RateValue)
		}
		return v, nil
	case "fixed":
		return r.RateValue, nil
	}
	return 0, nil
}

// periodEnded reports whether ev falls after the program's commission
// period for this customer and affiliate. The period starts when the
// customer was bound to the affiliate (signup through the link), or, for
// an affiliate credited without holding the binding (a coupon), at its
// first commission on the customer. No start yet = this is the first
// sale, which always earns. Events without a customer id cannot be
// followed across payments and are never capped.
func (e *Engine) periodEnded(ctx context.Context, ev Event, affiliateID int64) (bool, error) {
	if ev.CustomerExtID == "" {
		return false, nil
	}
	months := settings.CommissionMonths(ctx, e.db, ev.CampaignID)
	if months == 0 {
		return false, nil
	}
	var raw any
	err := e.db.QueryRowContext(ctx, `
		SELECT bound_at FROM customers
		WHERE program_id = ? AND external_customer_id = ? AND affiliate_id = ?`,
		CustomerNamespace, ev.CustomerExtID, affiliateID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = e.db.QueryRowContext(ctx, `
			SELECT MIN(created_at) FROM commissions
			WHERE affiliate_id = ? AND customer_ext_id = ? AND campaign_id = ?`,
			affiliateID, ev.CustomerExtID, ev.CampaignID).Scan(&raw)
	}
	if err != nil {
		return false, fmt.Errorf("commissions: period start: %w", err)
	}
	start, ok := dbutil.AsTime(raw)
	if !ok {
		return false, nil // NULL MIN: no earlier commission, first sale
	}
	at := ev.OccurredAt
	if at.IsZero() {
		at = e.now()
	}
	return !at.UTC().Before(start.AddDate(0, months, 0)), nil
}

// BindCustomer writes the lifetime customer binding without a credit: the
// same once-per-customer, first-touch, immutable row a credited purchase
// writes (an existing binding always wins). Billing adapters use it when
// the buyer's attribution signal arrives on an event that credits nothing
// itself (a subscription checkout; the invoices credit). No money moves,
// so there is nothing to screen: each commission the binding later earns
// is screened as it credits. campaignID is the program the binding's
// renewals credit in (<= 0 means program 1).
func (e *Engine) BindCustomer(ctx context.Context, customerExtID string, affiliateID, campaignID int64, method string) error {
	return bindCustomer(ctx, e.db, customerExtID, affiliateID, campaignID, method)
}

// CustomerNamespace is customers.program_id: the namespace a customer is
// bound in (one per store, so a customer has one owner across programs).
// It is not a program: the binding's program is customers.campaign_id,
// and every program lookup goes through it (PRD 12.4 item 4, FR-83 AC3).
const CustomerNamespace = 1

func bindCustomer(ctx context.Context, q dbtx, customerExtID string, affiliateID, campaignID int64, method string) error {
	if customerExtID == "" || affiliateID <= 0 {
		return nil
	}
	if campaignID <= 0 {
		campaignID = 1
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO customers (program_id, external_customer_id, affiliate_id, bound_method, campaign_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(program_id, external_customer_id) DO NOTHING`,
		CustomerNamespace, customerExtID, affiliateID, method, campaignID)
	if err != nil {
		return fmt.Errorf("commissions: bind customer: %w", err)
	}
	return nil
}

// bumpDaily records the credit under the RESOLVED affiliate: ev.AffiliateID
// is 0 when the engine's coupon/customer fallback attributed the sale.
// It runs in the money transaction, so its error is the transaction's: on
// Postgres a failed statement aborts the transaction it runs in. The
// money is in the row of the sale's currency (never added to another
// currency's).
func bumpDaily(ctx context.Context, q dbtx, ev Event, affiliateID, amount int64) error {
	day := ev.OccurredAt.UTC().Format("20060102")
	d, _ := Int(day)
	if _, err := q.ExecContext(ctx, `
		INSERT INTO daily_agg (day, affiliate_id, campaign_id, currency, clicks, conversions, commission_cents)
		VALUES (?, ?, ?, ?, 0, 1, ?)
		ON CONFLICT(day, affiliate_id, campaign_id, currency)
		DO UPDATE SET conversions = daily_agg.conversions + 1, commission_cents = daily_agg.commission_cents + ?`,
		d, affiliateID, ev.CampaignID, ev.Currency, amount, amount); err != nil {
		return fmt.Errorf("commissions: daily aggregate: %w", err)
	}
	return nil
}

// emitOutbox writes the outbox event in the money transaction: the row and
// its notification commit together or not at all.
func emitOutbox(ctx context.Context, q dbtx, topic string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("commissions: outbox %s: %w", topic, err)
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO outbox_events (topic, payload) VALUES (?, ?)`, topic, string(b)); err != nil {
		return fmt.Errorf("commissions: outbox %s: %w", topic, err)
	}
	return nil
}

// markProcessed finishes the event with a skip note (recorded and ACKed).
func markProcessed(ctx context.Context, q dbtx, ev Event, note string) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE raw_events SET processed_at = ?, process_error = ? WHERE source = ? AND external_event_id = ?`,
		time.Now().UTC(), note, ev.Source, ev.ExternalID); err != nil {
		return fmt.Errorf("commissions: mark processed: %w", err)
	}
	return nil
}

// markProcessedOK finishes the event.
func markProcessedOK(ctx context.Context, q dbtx, ev Event) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE raw_events SET processed_at = ? WHERE source = ? AND external_event_id = ?`,
		time.Now().UTC(), ev.Source, ev.ExternalID); err != nil {
		return fmt.Errorf("commissions: mark processed: %w", err)
	}
	return nil
}

// nullableStr maps "" -> SQL NULL for optional link columns.
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// Int parses a decimal string (import-cycle-free strconv for YYYYMMDD days).
func Int(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// dayOf extracts YYYYMMDD from a time (the mlm_volume_daily key).
func dayOf(t time.Time) int64 {
	utc := t.UTC()
	return int64(utc.Year())*10000 + int64(utc.Month())*100 + int64(utc.Day())
}
