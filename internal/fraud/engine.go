// Package fraud evaluates detection rules whose verdicts BIND TO MONEY:
// a flagged commission goes on hold (never auto-paid while under review).
// Ruleset v1 from the threat model (research/final/security-threat-model.md §4).
//
// Hold scope (anti-weaponisation): a verdict holds ONLY the commission
// under evaluation (Query.CommissionID) plus, for click-based rules, the
// commissions credited from the flagged clicks (Verdict.ClickIDs). It
// never blanket-holds every open commission of the affiliate — otherwise
// anyone able to aim junk traffic or a lookalike purchase at a victim's
// links could freeze the victim's whole balance. The account-freeze
// action is honored only for the high-confidence coupon_hijack rule and
// lands on the coupon's owner; any other ActionFreeze degrades to a hold.
package fraud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Verdict is one rule's decision on a subject.
type Verdict struct {
	Rule    string
	Subject string // affiliate id / coupon code / ip hash (hex)
	Reason  string
	Action  Action
	// ClickIDs are the flagged clicks of a click-based rule: commissions
	// credited from these clicks are held alongside the evaluated one.
	ClickIDs []string
}

// Action is what the verdict demands.
type Action int

const (
	ActionNone   Action = iota // informational only
	ActionHold                 // hold the evaluated (and click-linked) commissions for review
	ActionFreeze               // freeze the affiliate account (honored for coupon_hijack only)
)

// Rule evaluates one subject area. Dry-run mode scores without acting.
type Rule interface {
	Name() string
	Evaluate(ctx context.Context, q Query) ([]Verdict, error)
}

// Query is the shared evaluation input.
type Query struct {
	// CommissionID is the commission under evaluation. 0 (the periodic
	// sweep) records verdicts but can only hold click-linked commissions.
	CommissionID int64
	AffiliateID  int64
	// ProgramID scopes program-bound lookups (customer bindings): the
	// evaluated commission's campaign. 0 means program 1.
	ProgramID  int64
	Email      string
	IPHash     []byte
	CouponCode string
	DryRun     bool
	Now        time.Time
}

func (q Query) program() int64 {
	if q.ProgramID <= 0 {
		return 1
	}
	return q.ProgramID
}

// maxClickHold bounds how many flagged clicks one evaluation fans holds
// out to.
const maxClickHold = 200

// Engine runs all registered rules and applies verdicts.
type Engine struct {
	db    *sql.DB
	rules []Rule
}

// NewEngine registers the default ruleset.
func NewEngine(db *sql.DB) *Engine {
	return &Engine{db: db, rules: defaultRules(db)}
}

// NewEngineWithRules builds an engine over an explicit ruleset (tests,
// shadow rulesets).
func NewEngineWithRules(db *sql.DB, rules ...Rule) *Engine {
	return &Engine{db: db, rules: rules}
}

// Rules returns a copy of the registered ruleset.
func (e *Engine) Rules() []Rule { return append([]Rule(nil), e.rules...) }

// SetCouponHijackThreshold overrides the coupon_hijack trip count
// (overrides per coupon per 7 days). n <= 0 restores the default
// (DefaultCouponHijackThreshold). Call before the engine is shared.
func (e *Engine) SetCouponHijackThreshold(n int) {
	for i, r := range e.rules {
		if c, ok := r.(CouponHijackPattern); ok {
			c.threshold = n
			e.rules[i] = c
		}
	}
}

// DefaultRules returns the ported ruleset bound to db (the Shopify
// pipeline appends its tenant-evidence rules to these).
func DefaultRules(db *sql.DB) []Rule {
	return defaultRules(db)
}

func defaultRules(db *sql.DB) []Rule {
	return []Rule{
		SelfReferral{db},
		ClickVelocity{db},
		ConversionVelocity{db},
		CouponHijackPattern{db: db},
		ConversionTimeAnomaly{db},
		ASNReputation{db},
		IPOverlap{db},
		DuplicateAccount{db},
		PaymentFingerprint{db},
		BotClassGating{db},
		GeoMismatch{db},
		IPFallbackAnomaly{db},
		InactiveAffiliate{db},
	}
}

// Evaluate runs EVERY rule: one failing rule no longer short-circuits the
// rest of the ruleset and the hold step (the old early return made any
// broken query a fail-open switch). Verdicts from the rules that did
// succeed are recorded and, outside dry-run, applied; rule, record and
// apply errors come back joined so the caller can fail closed.
func (e *Engine) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	var all []Verdict
	var errs []error
	// Inside a transaction (WithTx) each step runs under a savepoint, so
	// one failing statement is undone alone and the steps after it still
	// run, as they do on the pool.
	for _, r := range e.rules {
		var vs []Verdict
		err := contained(ctx, func() error {
			var err error
			vs, err = r.Evaluate(ctx, q)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("fraud: %s: %w", r.Name(), err))
			continue
		}
		all = append(all, vs...)
	}
	for _, v := range all {
		if err := contained(ctx, func() error { return e.record(ctx, v, q.DryRun) }); err != nil {
			errs = append(errs, fmt.Errorf("fraud: record %s: %w", v.Rule, err))
		}
	}
	if !q.DryRun {
		if err := contained(ctx, func() error { return e.apply(ctx, all, q) }); err != nil {
			errs = append(errs, err)
		}
	}
	return all, errors.Join(errs...)
}

func (e *Engine) record(ctx context.Context, v Verdict, dry bool) error {
	_, err := Conn(ctx, e.db).ExecContext(ctx, `
		INSERT INTO fraud_verdicts (rule, subject, reason, action, dry_run, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		v.Rule, v.Subject, v.Reason, int(v.Action), dry, time.Now().UTC().Format(time.RFC3339))
	return err
}

// apply binds verdicts to money. Hold = payment block, NOT a state
// reversal (the commission state machine forbids approved->pending, and
// the ledger must not be mutated by fraud review). Payout batching
// excludes held rows.
func (e *Engine) apply(ctx context.Context, vs []Verdict, q Query) error {
	var rules, flagged, freezeCoupons []string
	seen := map[string]bool{}
	for _, v := range vs {
		if v.Action == ActionNone {
			continue
		}
		rules = append(rules, v.Rule)
		for _, id := range v.ClickIDs {
			if id != "" && !seen[id] && len(flagged) < maxClickHold {
				seen[id] = true
				flagged = append(flagged, id)
			}
		}
		// An ACCOUNT freeze is honored only for coupon_hijack: the signal
		// is the affiliate's own coupon overriding other affiliates'
		// customer bindings, which a third party cannot manufacture
		// against them. It lands on the coupon's owner. Other rules'
		// ActionFreeze (duplicate_account: anyone can sign up with a
		// victim's alias) is applied as a hold only.
		if v.Action == ActionFreeze && v.Rule == "coupon_hijack" {
			freezeCoupons = append(freezeCoupons, v.Subject)
		}
	}
	if len(rules) == 0 {
		return nil
	}
	reason := "fraud review: " + strings.Join(rules, ",")
	var errs []error
	if q.CommissionID != 0 {
		if err := e.HoldCommission(ctx, q.CommissionID, reason); err != nil {
			errs = append(errs, err)
		}
	}
	for _, id := range flagged {
		if _, err := Conn(ctx, e.db).ExecContext(ctx, `
			INSERT INTO commission_holds (commission_id, reason)
			SELECT c.id, CAST(? AS TEXT) FROM commissions c
			WHERE c.click_id = ? AND c.status IN ('pending','approved')
			  AND NOT EXISTS (SELECT 1 FROM commission_holds h WHERE h.commission_id = c.id)`,
			reason, id); err != nil {
			errs = append(errs, fmt.Errorf("fraud: click-linked hold %s: %w", id, err))
			break
		}
	}
	for _, code := range freezeCoupons {
		if _, err := Conn(ctx, e.db).ExecContext(ctx, `
			UPDATE affiliates SET status = 'frozen'
			WHERE status = 'active' AND id IN (SELECT affiliate_id FROM coupons WHERE code = ?)`,
			code); err != nil {
			errs = append(errs, fmt.Errorf("fraud: freeze coupon owner: %w", err))
		}
	}
	return errors.Join(errs...)
}

// HoldCommission places a payment hold on one pending/approved
// commission. Idempotent: a commission that already has a hold row —
// open, released or upheld — is left alone (a reviewer's release is not
// silently re-opened, and the commission_holds primary key never trips).
func (e *Engine) HoldCommission(ctx context.Context, commissionID int64, reason string) error {
	if _, err := Conn(ctx, e.db).ExecContext(ctx, `
		INSERT INTO commission_holds (commission_id, reason)
		SELECT c.id, CAST(? AS TEXT) FROM commissions c
		WHERE c.id = ? AND c.status IN ('pending','approved')
		  AND NOT EXISTS (SELECT 1 FROM commission_holds h WHERE h.commission_id = c.id)`,
		reason, commissionID); err != nil {
		return fmt.Errorf("fraud: hold commission %d: %w", commissionID, err)
	}
	return nil
}

// CommissionContext is what screening needs to evaluate a credited
// commission: who earned it, under which program, and the identity of the
// originating click.
type CommissionContext struct {
	AffiliateID int64
	CampaignID  int64
	ClickID     string
	IPHash      []byte // originating click's clicks.ip_hash (nil = unknown)
}

// LookupCommission loads a credited commission's evaluation context.
func (e *Engine) LookupCommission(ctx context.Context, commissionID int64) (CommissionContext, error) {
	var cc CommissionContext
	var click sql.NullString
	if err := Conn(ctx, e.db).QueryRowContext(ctx,
		`SELECT affiliate_id, campaign_id, click_id FROM commissions WHERE id = ?`, commissionID).
		Scan(&cc.AffiliateID, &cc.CampaignID, &click); err != nil {
		return cc, fmt.Errorf("fraud: lookup commission %d: %w", commissionID, err)
	}
	cc.ClickID = click.String
	if cc.ClickID != "" {
		err := Conn(ctx, e.db).QueryRowContext(ctx,
			`SELECT ip_hash FROM clicks WHERE id = ?`, cc.ClickID).Scan(&cc.IPHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return cc, fmt.Errorf("fraud: lookup click %s: %w", cc.ClickID, err)
		}
	}
	return cc, nil
}

// queryClickIDs runs a bounded click-id query for a click-based verdict.
func queryClickIDs(ctx context.Context, db Querier, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// minClickRuleConversions: the click-share rules (asn_reputation,
// bot_class_gating, ip_overlap) only produce an enforcing verdict once the
// affiliate has at least this many attributed conversions in the rule's
// window. Clicks cost an attacker nothing; without the floor, junk
// traffic aimed at a victim's links alone would put the victim on hold.
const minClickRuleConversions = 3

func enoughConversions(ctx context.Context, db Querier, affiliateID int64, since time.Time) (bool, error) {
	var n int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commissions
		WHERE affiliate_id = ? AND amount > 0 AND created_at > ?`,
		affiliateID, since.UTC().Format(time.RFC3339)).Scan(&n); err != nil {
		return false, err
	}
	return n >= minClickRuleConversions, nil
}

// InactiveAffiliate holds a commission credited to a frozen or removed
// affiliate. Attribution and the commissions engine's coupon/customer
// fallback refuse non-active affiliates, but an event that names the
// affiliate explicitly (resolved before a freeze landed) is not
// re-checked: this rule is the money-side backstop. Evaluated commissions
// only (the sweep has nothing to hold).
type InactiveAffiliate struct{ db *sql.DB }

func (InactiveAffiliate) Name() string { return "inactive_affiliate" }

func (r InactiveAffiliate) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.CommissionID == 0 || q.AffiliateID == 0 {
		return nil, nil
	}
	var status string
	err := Conn(ctx, r.db).QueryRowContext(ctx,
		`SELECT status FROM affiliates WHERE id = ?`, q.AffiliateID).Scan(&status)
	if err == sql.ErrNoRows || (err == nil && status == "active") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []Verdict{{
		Rule: "inactive_affiliate", Subject: fmt.Sprint(q.AffiliateID),
		Reason: fmt.Sprintf("A commission was credited to a partner whose account is %s", partnerStatusWord(status)),
		Action: ActionHold,
	}}, nil
}

// --- Rule 1: self-referral — buyer email matches affiliate (normalized) ---

type SelfReferral struct{ db *sql.DB }

func (SelfReferral) Name() string { return "self_referral" }

func (r SelfReferral) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.Email == "" {
		return nil, nil
	}
	var email string
	err := Conn(ctx, r.db).QueryRowContext(ctx,
		`SELECT email FROM affiliates WHERE id = ?`, q.AffiliateID).Scan(&email)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if normalizeEmail(email) == normalizeEmail(q.Email) {
		return []Verdict{{
			Rule: "self_referral", Subject: fmt.Sprint(q.AffiliateID),
			Reason: fmt.Sprintf("The buyer's email %q is the partner's own email", q.Email),
			Action: ActionHold,
		}}, nil
	}
	return nil, nil
}

// normalizeEmail: lowercase, strip plus-aliases (foo+tag@ -> foo@), trim dots
// on gmail (the common alias trick).
func normalizeEmail(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return e
	}
	local, domain := e[:at], e[at+1:]
	if plus := strings.Index(local, "+"); plus > 0 {
		local = local[:plus]
	}
	if domain == "gmail.com" || domain == "googlemail.com" {
		local = strings.ReplaceAll(local, ".", "")
		if domain == "googlemail.com" {
			domain = "gmail.com"
		}
	}
	return local + "@" + domain
}

// NormalizeEmail exposes the alias normalization for erasure matching
// (privacy scrubbing must find the same string duplicate_account quotes).
func NormalizeEmail(e string) string { return normalizeEmail(e) }

// --- Rule 2: click velocity — clicks/IP/hour over threshold ---

type ClickVelocity struct{ db *sql.DB }

func (ClickVelocity) Name() string { return "click_velocity" }

func (r ClickVelocity) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.IPHash == nil {
		return nil, nil
	}
	since := q.Now.UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	var n int
	err := Conn(ctx, r.db).QueryRowContext(ctx, `
		SELECT COUNT(*) FROM clicks
		WHERE ip_hash = ? AND exclude_stats = 0
		  AND ts > ?`,
		q.IPHash, since).Scan(&n)
	if err != nil {
		return nil, err
	}
	if n < 50 { // threshold from the benchmark fraud bursts (205/hr max legit-scale guard)
		return nil, nil
	}
	// Flagged clicks: the burst itself, scoped to the evaluated affiliate
	// when known (their commissions credited from it get held).
	var ids []string
	if q.AffiliateID != 0 {
		ids, err = queryClickIDs(ctx, Conn(ctx, r.db), `
			SELECT id FROM clicks
			WHERE ip_hash = ? AND exclude_stats = 0 AND ts > ? AND affiliate_id = ?
			LIMIT 200`, q.IPHash, since, q.AffiliateID)
	} else {
		ids, err = queryClickIDs(ctx, Conn(ctx, r.db), `
			SELECT id FROM clicks
			WHERE ip_hash = ? AND exclude_stats = 0 AND ts > ?
			LIMIT 200`, q.IPHash, since)
	}
	if err != nil {
		return nil, err
	}
	return []Verdict{{
		Rule: "click_velocity", Subject: fmt.Sprintf("ip:%x", q.IPHash),
		Reason:   fmt.Sprintf("%d clicks from the same visitor in the last hour", n),
		Action:   ActionHold,
		ClickIDs: ids,
	}}, nil
}

// --- Rule 3: conversion velocity — suspicious fast conversions for affiliate ---

type ConversionVelocity struct{ db *sql.DB }

func (ConversionVelocity) Name() string { return "conversion_velocity" }

func (r ConversionVelocity) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	var n int
	err := Conn(ctx, r.db).QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commissions
		WHERE affiliate_id = ? AND created_at > ?`,
		q.AffiliateID, q.Now.UTC().Add(-time.Hour).Format(time.RFC3339)).Scan(&n)
	if err != nil {
		return nil, err
	}
	if n >= 30 {
		return []Verdict{{
			Rule: "conversion_velocity", Subject: fmt.Sprint(q.AffiliateID),
			Reason: fmt.Sprintf("%d sales credited within one hour", n),
			Action: ActionHold,
		}}, nil
	}
	return nil, nil
}

// --- Rule 4: coupon-hijack pattern — one coupon redeemed by many distinct
// customers who ALREADY had attribution to a different affiliate (the
// Honey signature). Rewritten: the original query grepped raw_events
// payloads for '"method":"token"', a key that never exists in the
// marshalled Event — it could not fire. The signal now reads persisted
// provenance: commissions credited via method='coupon' whose customer is
// durably bound (customers table) to a DIFFERENT affiliate — i.e., the
// coupon overrode an existing referral relationship. Bindings are read in
// the evaluated commission's program (was hard-coded to program 1). ---

// DefaultCouponHijackThreshold is the per-coupon override count (7d) that
// trips the rule. Was 10/week: by then a leaked coupon has already been
// credited on several stolen customers.
const DefaultCouponHijackThreshold = 3

type CouponHijackPattern struct {
	db        *sql.DB
	threshold int // <= 0: DefaultCouponHijackThreshold
}

func (CouponHijackPattern) Name() string { return "coupon_hijack" }

func (r CouponHijackPattern) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.CouponCode == "" {
		return nil, nil
	}
	threshold := r.threshold
	if threshold <= 0 {
		threshold = DefaultCouponHijackThreshold
	}
	var hijacked int
	err := Conn(ctx, r.db).QueryRowContext(ctx, `
		SELECT COUNT(*) FROM commissions c
		JOIN customers cu
		  ON cu.program_id = ? AND cu.external_customer_id = c.customer_ext_id
		WHERE c.attribution_method = 'coupon'
		  AND c.coupon_code = ?
		  AND c.created_at > ?
		  AND cu.affiliate_id != c.affiliate_id
		  AND cu.bound_method <> 'manual'`,
		q.program(), q.CouponCode, q.Now.UTC().Add(-7*24*time.Hour).Format(time.RFC3339)).Scan(&hijacked)
	if err != nil {
		return nil, err
	}
	if hijacked >= threshold {
		return []Verdict{{
			Rule: "coupon_hijack", Subject: q.CouponCode,
			Reason: fmt.Sprintf("Coupon %q took %d existing customers from other partners in the last 7 days", q.CouponCode, hijacked),
			Action: ActionFreeze,
		}}, nil
	}
	return nil, nil
}

// partnerStatusWord names an affiliate status the way the dashboard does.
func partnerStatusWord(status string) string {
	switch status {
	case "frozen":
		return "paused"
	case "pending":
		return "still pending approval"
	default:
		return status
	}
}
