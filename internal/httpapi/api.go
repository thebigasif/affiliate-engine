// Package httpapi: JSON API surface (FR-29) serving the admin console and
// affiliate portal reads (FR-27/28). Auth is scoped hashed API keys; every
// money number surfaced here is derived from the ledger or the commissions
// state machine — never a parallel counter.
package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/analytics"
	"github.com/thebigasif/affiliate-engine/internal/apikeys"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/money"
	"github.com/thebigasif/affiliate-engine/internal/partnermail"
	"github.com/thebigasif/affiliate-engine/internal/payouts"
	"github.com/thebigasif/affiliate-engine/internal/plan"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// authFailPerMin caps failed API-key attempts per client IP per minute.
// Checked BEFORE the key lookup, so a guessing loop is cut off without
// touching the database.
const authFailPerMin = 30

// APIDeps wires the API to its dependencies.
type APIDeps struct {
	DB           *sql.DB
	PayoutRunner *payouts.Runner // may be nil (payout-run endpoint disabled)

	// PayoutExecutor moves money for Approve (nil = manual rail: CSV export
	// + operator pays; the ledger still settles payable -> clearing).
	PayoutExecutor func(payoutID int64, amount int64) error
	RateLimit      int          // requests per minute per key; default 600
	LedgerPoster   Poster       // may be nil (fraud uphold runs without clawback)
	MLM            MLMEngine    // may be nil (mlm endpoints disabled)
	Gamify         GamifyEngine // may be nil (profile/leaderboard disabled)
	Stats          StatsEngine  // may be nil (drill endpoint disabled)

	// TrustedProxies: peers allowed to supply X-Forwarded-For /
	// X-Forwarded-Proto (client IP for rate limits, Secure detection).
	TrustedProxies []netip.Prefix
	// SecureCookies forces Secure on the portal session cookie (TLS
	// requests get it regardless).
	SecureCookies bool

	// Plan gates cloud-plan features (AE_PLAN). nil = self-host: all on.
	Plan *plan.Plan

	// Demo serves the public read-only demo (AE_DEMO=1): writes refused,
	// only DemoKey authenticates (demo.go).
	Demo bool

	// OwnerEmail is where the owner's "something needs you" email goes
	// unless Settings says otherwise (AE_OWNER_EMAIL; the account email on
	// Cloud).
	OwnerEmail string

	// SiteURL is the owner's website (the redirect fallback), prefilled in
	// "Test your setup". SiteFetch fetches it (nil = the SSRF-guarded
	// client; tests override).
	SiteURL   string
	SiteFetch *http.Client

	// TaxDocKey seals uploaded tax forms (32 bytes; DeriveTaxDocKey of the
	// deployment secret). nil = portal uploads refused (503).
	TaxDocKey []byte

	// MailOn: partner emails are configured (AE_MAIL_PROVIDER). Approvals
	// and approved payouts queue an email, and affiliates can ask for a
	// login link by email (partnermail).
	MailOn bool

	// NewsKey signs partners' unsubscribe links for program news
	// (partnermail.DeriveNewsKey). nil = news sending off.
	NewsKey []byte

	// Billing names the billing webhooks this engine accepts (stripe,
	// paddle, ...), for the dashboard's setup list.
	Billing []string
}

// StatsEngine is the token-analytics slice the API needs.
type StatsEngine interface {
	Drill(ctx context.Context, affiliateID int64, dim string, fromDay, toDay int64, currency string, limit int) ([]analytics.DrillRow, error)
}

// GamifyEngine is the slice of *gamify.Engine the API needs.
type GamifyEngine interface {
	Profile(ctx context.Context, affiliateID int64) (map[string]any, error)
	Leaderboard(ctx context.Context, limit int) ([]map[string]any, error)
}

// MLMEngine is the slice of *mlm.Engine the API needs.
type MLMEngine interface {
	PlaceInSponsorTree(ctx context.Context, id, sponsorID int64) error
	PlaceInPlacementTree(ctx context.Context, id, parentID int64, leg string) error
	RunPeriod(ctx context.Context, start, end time.Time) (int64, error)
}

// Poster posts journal lines (satisfied by *ledger.Poster). Run commits a
// journal together with the domain rows it pays for (fraud uphold, usage
// settlement).
type Poster interface {
	Post(ctx context.Context, idemKey, source, description string, effective time.Time, lines []ledger.Line) (int64, error)
	Run(ctx context.Context, fn func(ctx context.Context, tx *ledger.Tx) error) error
}

// NewAPIHandler builds the /api/v1 mux (auth enforced on every route).
func NewAPIHandler(deps APIDeps) http.Handler {
	a := &api{
		db: deps.DB, deps: deps, rl: newRateLimiter(deps.RateLimit), stats: deps.Stats,
		authFail:   newRateLimiter(authFailPerMin),
		loginLinks: newLoginLinkLimiter(5 * time.Minute),
	}
	mux := http.NewServeMux()

	// Affiliate-scope routes (own data only).
	mux.HandleFunc("GET /api/v1/affiliate/earnings", a.aff(a.handleEarnings))
	mux.HandleFunc("GET /api/v1/affiliate/profile", a.aff(a.handleGamifyProfile))
	mux.HandleFunc("GET /api/v1/affiliate/ledger", a.aff(a.handleLedgerView))
	mux.HandleFunc("GET /api/v1/affiliate/payouts", a.aff(a.handleAffiliatePayouts))
	mux.HandleFunc("GET /api/v1/affiliate/payout-details", a.aff(a.handleAffiliatePayoutDetails))
	mux.HandleFunc("PUT /api/v1/affiliate/payout-details", a.sameOrigin(a.aff(a.handleAffiliatePayoutDetails)))
	mux.HandleFunc("GET /api/v1/affiliate/stats/drill", a.aff(a.handleTokenDrill))
	mux.HandleFunc("GET /api/v1/affiliate/uniques", a.aff(a.handleUniques))

	// Public portal routes (no auth; rate-limited).
	mux.HandleFunc("POST /api/v1/portal/apply", a.public(a.handlePortalApply))
	mux.HandleFunc("POST /api/v1/portal/login", a.public(a.sameOrigin(a.handlePortalLoginQ)))
	mux.HandleFunc("POST /api/v1/portal/logout", a.public(a.sameOrigin(a.handlePortalLogout)))
	mux.HandleFunc("POST /api/v1/portal/login-link", a.public(a.handlePortalLoginLink))
	mux.HandleFunc("GET /api/v1/portal/unsubscribe", a.public(a.handleUnsubscribe))
	mux.HandleFunc("POST /api/v1/portal/unsubscribe", a.public(a.sameOrigin(a.handleUnsubscribe)))
	mux.HandleFunc("POST /api/v1/admin/partners/message", a.admin(a.handlePartnerMessage))
	mux.HandleFunc("GET /api/v1/leaderboard", a.public(a.handleGamifyLeaderboard))

	// Admin-scope routes.
	mux.HandleFunc("GET /api/v1/admin/dashboard", a.admin(a.handleDashboard))
	mux.HandleFunc("GET /api/v1/admin/affiliates", a.admin(a.handleAffiliates))
	mux.HandleFunc("GET /api/v1/admin/fraud/queue", a.admin(a.handleFraudQueue))
	mux.HandleFunc("POST /api/v1/admin/fraud/resolve", a.admin(a.handleFraudResolve))
	mux.HandleFunc("POST /api/v1/admin/payouts/run", a.admin(a.handlePayoutRun))
	mux.HandleFunc("POST /api/v1/admin/payouts/approve", a.admin(a.handlePayoutApprove))
	mux.HandleFunc("GET /api/v1/admin/portal/applications", a.admin(a.handlePortalQueue))
	mux.HandleFunc("POST /api/v1/admin/portal/review", a.admin(a.handlePortalReview))
	mux.HandleFunc("GET /api/v1/admin/campaigns", a.admin(a.handleCampaigns))
	mux.HandleFunc("POST /api/v1/admin/campaigns", a.admin(a.handleCampaigns))
	mux.HandleFunc("GET /api/v1/admin/rules", a.admin(a.handleRules))
	mux.HandleFunc("POST /api/v1/admin/rules", a.admin(a.handleRules))
	mux.HandleFunc("GET /api/v1/admin/tiers", a.admin(a.handleTiers))
	mux.HandleFunc("POST /api/v1/admin/tiers", a.admin(a.handleTiers))
	mux.HandleFunc("DELETE /api/v1/admin/tiers", a.admin(a.handleTiers))
	mux.HandleFunc("PATCH /api/v1/admin/affiliates/", a.admin(a.handleAffiliateUpdateQ))
	mux.HandleFunc("PUT /api/v1/admin/affiliates/{id}/rate", a.admin(a.handlePartnerRate))
	mux.HandleFunc("POST /api/v1/admin/setup-check/site", a.admin(a.handleSetupCheckSite))
	mux.HandleFunc("POST /api/v1/admin/setup-check/start", a.admin(a.handleSetupCheckStart))
	mux.HandleFunc("GET /api/v1/admin/setup-check", a.admin(a.handleSetupCheck))
	mux.HandleFunc("GET /api/v1/admin/resources", a.admin(a.handleResources))
	mux.HandleFunc("POST /api/v1/admin/resources", a.admin(a.handleResources))
	mux.HandleFunc("DELETE /api/v1/admin/resources", a.admin(a.handleResources))
	mux.HandleFunc("POST /api/v1/admin/resources/image", a.admin(a.handleResourceImage))
	mux.HandleFunc("GET /api/v1/affiliate/resources", a.aff(a.handleAffiliateResources))
	mux.HandleFunc("GET /api/v1/portal/resources/{id}/image", a.public(a.handleResourceImageGet))
	mux.HandleFunc("POST /api/v1/admin/usage", a.admin(a.feature(plan.FeatUsage, a.handleUsageIngest)))
	mux.HandleFunc("POST /api/v1/admin/mlm/run", a.admin(a.feature(plan.FeatMLM, a.handleMLMRun)))
	mux.HandleFunc("POST /api/v1/admin/mlm/place", a.admin(a.feature(plan.FeatMLM, a.handleMLMPlace)))
	mux.HandleFunc("GET /api/v1/admin/codes", a.admin(a.handleCodes))
	mux.HandleFunc("POST /api/v1/admin/codes", a.admin(a.handleCodes))
	mux.HandleFunc("DELETE /api/v1/admin/codes", a.admin(a.handleCodes))

	// Tax-doc gating (FR-17): admin records doc status; blocking statuses
	// gate payout batching + approval.
	mux.HandleFunc("GET /api/v1/admin/taxdocs", a.admin(a.handleTaxDocs))
	mux.HandleFunc("POST /api/v1/admin/taxdocs", a.admin(a.handleTaxDocs))
	mux.HandleFunc("GET /api/v1/admin/taxdocs/file", a.admin(a.handleTaxDocFile))
	// Portal tax-form submission: the affiliate uploads the PDF itself.
	mux.HandleFunc("GET /api/v1/affiliate/taxdocs", a.aff(a.handleAffiliateTaxDocs))
	mux.HandleFunc("POST /api/v1/affiliate/taxdocs", a.sameOrigin(a.aff(a.handleAffiliateTaxDocUpload)))

	// Payout console: list, manual-rail CSV export, program settings.
	mux.HandleFunc("GET /api/v1/admin/payouts", a.admin(a.handlePayoutList))
	mux.HandleFunc("GET /api/v1/admin/payouts/export/{rail}", a.admin(a.handlePayoutExport))
	mux.HandleFunc("POST /api/v1/admin/import", a.admin(a.handleImport))
	mux.HandleFunc("GET /api/v1/admin/import/batches", a.admin(a.handleImportBatches))
	mux.HandleFunc("POST /api/v1/admin/import/undo", a.admin(a.handleImportUndo))
	mux.HandleFunc("GET /api/v1/admin/settings", a.admin(a.handleSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings", a.admin(a.handleSettings))
	mux.HandleFunc("PATCH /api/v1/admin/campaigns/{id}", a.admin(a.handleCampaignUpdate))

	// White-label (FR-33): admin CRUD + public portal brand read.
	mux.HandleFunc("GET /api/v1/admin/branding", a.admin(a.handleBranding))
	mux.HandleFunc("PUT /api/v1/admin/branding", a.admin(a.feature(plan.FeatBranding, a.handleBranding)))
	mux.HandleFunc("PUT /api/v1/admin/branding/logo", a.admin(a.feature(plan.FeatBranding, a.handleBrandLogo)))
	mux.HandleFunc("DELETE /api/v1/admin/branding/logo", a.admin(a.handleBrandLogo))
	mux.HandleFunc("GET /api/v1/portal/brand", a.public(a.handlePortalBrand))
	mux.HandleFunc("GET /api/v1/portal/logo", a.public(a.handlePortalLogo))

	if deps.Demo {
		return DemoGuard(mux)
	}
	return mux
}

type api struct {
	db       *sql.DB
	deps     APIDeps
	rl       *rateLimiter
	authFail *rateLimiter // failed-auth attempts per client IP
	stats    StatsEngine

	loginLinks *loginLinkLimiter // login-link emails per address
}

// --- auth wrappers ---

func (a *api) aff(next func(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Portal sessions authenticate as the affiliate (cookie or bearer
		// aps_ token); scope pinned to the session's affiliate id. API keys
		// (ae_ prefix) go through the key path instead.
		// (Not in demo mode: only the demo key authenticates there.)
		bt := bearerToken(r)
		if !a.deps.Demo && (bt == "" || strings.HasPrefix(bt, "aps_")) {
			if affID := a.PortalAuth(r.Context(), r); affID != 0 {
				next(w, r, &apikeys.Identity{AffiliateID: affID, Scope: "affiliate:" + fmt.Sprint(affID)})
				return
			}
		}
		idn := a.auth(w, r)
		if idn == nil {
			return
		}
		if idn.IsAdmin || idn.AffiliateID != 0 {
			// Admin may read any affiliate's view (explicit ?affiliate_id=);
			// affiliate keys are pinned to their own id.
			next(w, r, idn)
			return
		}
		apiError(w, http.StatusForbidden, "affiliate scope required")
	}
}

// feature refuses a route the workspace's cloud plan does not include
// (402 with an upgrade hint). Self-host (nil plan) passes everything.
func (a *api) feature(f string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.deps.Plan.Allows(f) {
			apiError(w, http.StatusPaymentRequired, a.deps.Plan.Denied(f))
			return
		}
		next(w, r)
	}
}

func (a *api) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idn := a.auth(w, r)
		if idn == nil {
			return
		}
		if !idn.IsAdmin {
			apiError(w, http.StatusForbidden, "admin scope required")
			return
		}
		next(w, r)
	}
}

// auth authenticates the bearer key, enforces the rate limit, and writes
// the error response on failure. Returns nil on failure.
//
// Failed attempts are counted per client IP and checked BEFORE the key
// lookup: once an IP burns its failure budget for the minute, even its
// next guess never reaches the database.
func (a *api) auth(w http.ResponseWriter, r *http.Request) *apikeys.Identity {
	if a.deps.Demo {
		return a.demoAuth(w, r)
	}
	failBucket := "authfail:" + ClientIP(r, a.deps.TrustedProxies)
	if a.authFail.exceeded(failBucket) {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, "too many failed authentication attempts")
		return nil
	}
	key := bearerToken(r)
	idn, err := apikeys.Authenticate(r.Context(), a.db, key)
	if err != nil {
		a.authFail.allow(failBucket) // count the failure
		apiError(w, http.StatusUnauthorized, "invalid or missing API key")
		return nil
	}
	if !a.rl.allow(keyHash(key)) {
		apiError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return nil
	}
	return idn
}

// targetAffiliateID resolves which affiliate's data to serve: the caller's
// own id for affiliate keys, or ?affiliate_id= for admin keys.
func targetAffiliateID(r *http.Request, idn *apikeys.Identity) (int64, error) {
	if idn.IsAdmin {
		if v := r.URL.Query().Get("affiliate_id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id <= 0 {
				return 0, fmt.Errorf("bad affiliate_id")
			}
			return id, nil
		}
		return 0, fmt.Errorf("affiliate_id required for admin scope")
	}
	if idn.AffiliateID == 0 {
		return 0, fmt.Errorf("key has no affiliate scope")
	}
	return idn.AffiliateID, nil
}

// --- affiliate endpoints ---

// statusTotal is one commission status's count and sum, in one currency.
type statusTotal struct {
	Count int64 `json:"count"`
	Cents int64 `json:"cents"`
}

// currencyEarnings is an affiliate's money in ONE currency. Earnings in
// different currencies are never added together: the earnings endpoint
// lists one of these per currency.
type currencyEarnings struct {
	Currency            string                 `json:"currency"`
	Totals              map[string]statusTotal `json:"totals"`
	PayableBalanceCents int64                  `json:"payable_balance_cents"`
	UnpaidCents         int64                  `json:"unpaid_cents"`
	EarnedCents         int64                  `json:"earned_cents"`
	OwedCents           int64                  `json:"owed_cents"`
	Holds               bool                   `json:"holds"` // ledger proof: owed == -payable
}

func (a *api) handleEarnings(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	db := a.db
	progCur := settings.Currency(ctx, db)

	// Commission totals by currency and status.
	byCur := map[string]*currencyEarnings{}
	cur := func(c string) *currencyEarnings {
		c = money.Normalize(c)
		if byCur[c] == nil {
			byCur[c] = &currencyEarnings{Currency: c, Totals: map[string]statusTotal{}}
		}
		return byCur[c]
	}
	rows, err := db.QueryContext(ctx, `
		SELECT currency, status, COUNT(*), COALESCE(SUM(amount),0)
		FROM commissions WHERE affiliate_id = ? GROUP BY currency, status`, affID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	for rows.Next() {
		var c, s string
		var n, cents int64
		if err := rows.Scan(&c, &s, &n, &cents); err != nil {
			_ = rows.Close()
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		cur(c).Totals[s] = statusTotal{n, cents}
	}
	_ = rows.Close()

	// Clicks + conversions over the last 30 days from the aggregate.
	since := time.Now().UTC().AddDate(0, 0, -30).Format("20060102")
	var clicks30d, conversions30d int64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0)
		FROM daily_agg WHERE affiliate_id = ? AND day >= ?`, affID, since).
		Scan(&clicks30d, &conversions30d); err != nil {
		apiError(w, http.StatusInternalServerError, "agg query failed")
		return
	}

	daily, err := dailySeries(ctx, db, time.Now().UTC(), 30, affID, primaryCurrency(byCur, progCur))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "daily query failed")
		return
	}

	// The payable balances straight from the ledger (the trust numbers),
	// one account per currency.
	payable, err := sumsByCurrency(ctx, db, `
		SELECT la.currency, COALESCE(SUM(le.amount),0)
		FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.code LIKE ? ESCAPE '!' GROUP BY la.currency`, payableLike(affID))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "ledger query failed")
		return
	}

	// The trust proof (INV-API-1), per currency: what the commissions
	// table says this affiliate is owed right now must equal -payable.
	// Owed = approved commissions (full amount) plus every clawback row
	// whose money came out of the payable (parent approved, paid or clawed
	// after payout), minus anything already settled by a paid payout.
	// Pending parents and their clawbacks live in HOLD; cancelled parents
	// net to zero.
	owed, err := owedFromCommissions(ctx, db, affID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "proof query failed")
		return
	}
	for c, v := range payable {
		cur(c).PayableBalanceCents = v
	}
	for c, v := range owed {
		cur(c).OwedCents = v
	}
	holds := true
	currencies := make([]currencyEarnings, 0, len(byCur))
	for _, c := range sortedCurrencies(byCur) {
		e := byCur[c]
		// Unpaid = pending + approved: everything earned and not yet paid
		// out. Only part of it sits in the payable: pending money is still
		// in HOLD (it moves to the payable at maturation), so unpaid is NOT
		// the proof.
		e.UnpaidCents = e.Totals["pending"].Cents + e.Totals["approved"].Cents
		e.EarnedCents = e.Totals["paid"].Cents + e.UnpaidCents
		e.Holds = e.OwedCents == -e.PayableBalanceCents
		holds = holds && e.Holds
		currencies = append(currencies, *e)
	}

	// The single-number fields predate per-currency money: they report the
	// affiliate's primary currency (the program currency, or the one
	// currency it earns in), never a sum across currencies.
	primary := cur(primaryCurrency(byCur, progCur))
	totals := primary.Totals

	// FR-17 visibility: a blocking tax document (pending/rejected) is why
	// this affiliate's approved balance is not batching into payouts.
	// Empty string = nothing blocking.
	var blockedDoc string
	if err := db.QueryRowContext(ctx, `
		SELECT doc_type FROM tax_documents
		WHERE affiliate_id = ? AND status IN ('pending','rejected')
		ORDER BY id LIMIT 1`, affID).Scan(&blockedDoc); err != nil && !errors.Is(err, sql.ErrNoRows) {
		apiError(w, http.StatusInternalServerError, "taxdoc query failed")
		return
	}

	// Recent commissions.
	type commissionRow struct {
		ID        int64  `json:"id"`
		Amount    int64  `json:"amount_cents"`
		Currency  string `json:"currency"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
		MaturesAt string `json:"matures_at,omitempty"`
	}
	var recent []commissionRow = []commissionRow{}
	rows, err = db.QueryContext(ctx, `
		SELECT id, amount, currency, status, created_at, matures_at
		FROM commissions WHERE affiliate_id = ? ORDER BY id DESC LIMIT 50`, affID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "recent query failed")
		return
	}
	for rows.Next() {
		var cr commissionRow
		var matures sql.NullString
		if err := rows.Scan(&cr.ID, &cr.Amount, &cr.Currency, &cr.Status, &cr.CreatedAt, &matures); err != nil {
			_ = rows.Close()
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		cr.MaturesAt = matures.String // omitempty drops it when NULL
		recent = append(recent, cr)
	}
	_ = rows.Close()

	// EPC on 30d clicks (earnings per click, minor units of the primary
	// currency; clicks are not per currency, so other currencies' earnings
	// are left out rather than added in).
	epc := 0.0
	if clicks30d > 0 {
		epc = float64(primary.EarnedCents) / float64(clicks30d)
	}
	// CTR on the same window (FR-24: conversions / clicks). Basis points
	// avoided — a plain ratio keeps the portal math simple; consumers can
	// format. 0 clicks = 0, matching EPC's convention.
	ctr := 0.0
	if clicks30d > 0 {
		ctr = float64(conversions30d) / float64(clicks30d)
	}

	// Primary referral code for the link generator; "" = links use the id.
	var refCode string
	if err := db.QueryRowContext(ctx, `
		SELECT code FROM affiliate_codes WHERE affiliate_id = ?
		ORDER BY is_primary DESC, id LIMIT 1`, affID).Scan(&refCode); err != nil && err != sql.ErrNoRows {
		apiError(w, http.StatusInternalServerError, "code lookup failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"affiliate_id":             affID,
		"referral_code":            refCode,
		"deal":                     a.partnerDeal(ctx, affID),
		"currency":                 primary.Currency,
		"totals":                   totals,
		"daily":                    daily,
		"clicks_30d":               clicks30d,
		"conversions_30d":          conversions30d,
		"epc_cents_30d":            epc,
		"ctr_30d":                  ctr,
		"payable_balance_cents":    primary.PayableBalanceCents,
		"unpaid_cents":             primary.UnpaidCents,
		"payout_blocked_by_taxdoc": blockedDoc,
		"ledger_proof": map[string]any{
			"claim":      "approved commissions net of clawbacks, not yet paid out == -payable_balance, in every currency",
			"currency":   primary.Currency,
			"owed_cents": primary.OwedCents,
			"holds":      holds,
		},
		// One entry per currency the affiliate has money in, never summed.
		"currencies":         currencies,
		"recent_commissions": recent,
	})
}

// owedFromCommissions derives, from the commissions state machine alone,
// how much the affiliate's payable should hold in each currency (as a
// positive number). It mirrors every path that touches
// AFFILIATE_PAYABLE:<id>:<CCY>:
//
//   - maturation moves an approved parent's unclawed remainder in; a
//     clawback taken while the parent was pending came out of HOLD, so
//     counting the parent in full and that clawback row negatively nets
//     to exactly the remainder that moved;
//   - clawbacks of approved/paid parents come straight out of the payable
//     (a post-payout refund can take it positive: owed goes negative);
//   - a paid payout settles its lines (parents at full amount, netted
//     clawbacks negative), so rows in a paid payout drop out;
//   - a cancelled parent (fraud uphold or full refund) and its clawbacks
//     net to zero on both sides.
func owedFromCommissions(ctx context.Context, db *sql.DB, affID int64) (map[string]int64, error) {
	return sumsByCurrency(ctx, db, `
		SELECT c.currency, COALESCE(SUM(c.amount), 0)
		FROM commissions c
		LEFT JOIN commissions p ON p.id = c.parent_commission_id
		WHERE c.affiliate_id = ?
		  AND NOT EXISTS (SELECT 1 FROM payout_lines pl
		                  JOIN payouts po ON po.id = pl.payout_id
		                  WHERE pl.commission_id = c.id AND po.status = 'paid')
		  AND ((c.parent_commission_id IS NULL AND c.status = 'approved')
		    OR (c.parent_commission_id IS NOT NULL AND c.amount < 0
		        AND p.status IN ('approved','paid','clawback')))
		GROUP BY c.currency`, affID)
}

// handleLedgerView serves the auditable ledger view (FR-28): every journal
// that touched this affiliate's payable account in ONE currency, in id
// order, each with the full-journal line count and sum (the Σ=0 proof).
// ?currency= picks the account (default: the primary currency, as the
// earnings endpoint's single-number fields); "currencies" lists every
// currency the affiliate has a payable in.
func (a *api) handleLedgerView(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	db := a.db
	held, err := sumsByCurrency(ctx, db, `
		SELECT currency, COUNT(*) FROM ledger_accounts
		WHERE code LIKE ? ESCAPE '!' GROUP BY currency`, payableLike(affID))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "ledger query failed")
		return
	}
	currency := primaryCurrency(held, settings.Currency(ctx, db))
	if q := r.URL.Query().Get("currency"); q != "" {
		currency = money.Normalize(q)
		if !money.Valid(currency) {
			apiError(w, http.StatusBadRequest, "currency must be a 3-letter ISO 4217 code")
			return
		}
	}
	code := ledger.Payable(affID, currency)

	// Keyset pagination (audit P1-8): the unbounded scan streamed the
	// account's entire history per hit — O(all-time) for a 5-year
	// affiliate. Cursor = last journal id; default page 200, max 1000.
	cursor := int64(0)
	if s := r.URL.Query().Get("cursor"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			cursor = v
		}
	}
	limit := 200
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT je.id, je.source, je.description, je.effective_at,
		       le.amount,
		       (SELECT COUNT(*) FROM ledger_entries x WHERE x.journal_id = je.id) AS n_lines,
		       (SELECT COALESCE(SUM(x.amount),0) FROM ledger_entries x WHERE x.journal_id = je.id) AS j_sum
		FROM ledger_entries le
		JOIN journal_entries je ON je.id = le.journal_id
		JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.code = ? AND je.id > ?
		ORDER BY je.id
		LIMIT ?`, code, cursor, limit+1)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "ledger query failed")
		return
	}
	defer rows.Close()

	type entry struct {
		JournalID   int64  `json:"journal_id"`
		Source      string `json:"source"`
		Description string `json:"description"`
		EffectiveAt string `json:"effective_at"`
		AmountCents int64  `json:"amount_cents"` // on this affiliate's payable
		Lines       int64  `json:"journal_lines"`
		JournalSum  int64  `json:"journal_sum_cents"` // must be 0 — the proof
	}
	var entries []entry = []entry{}
	var running int64
	allSumZero := true
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.JournalID, &e.Source, &e.Description, &e.EffectiveAt,
			&e.AmountCents, &e.Lines, &e.JournalSum); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if e.JournalSum != 0 || e.Lines < 2 {
			allSumZero = false
		}
		running += e.AmountCents
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}

	// Peek row: fetched limit+1; if present, more pages exist.
	var next int64
	if len(entries) > limit {
		next = entries[limit-1].JournalID
		entries = entries[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"affiliate_id": affID,
		"currency":     currency,
		"currencies":   sortedCurrencies(held),
		"entries":      entries,
		// Page-scoped: the running balance of THIS page (balance proofs
		// live in the totals endpoints; full-history sums remain the
		// property tests' domain).
		"page_sum_cents": running,
		"next_cursor":    next, // 0 = no more pages
		"proof": map[string]any{
			"all_journals_sum_zero": allSumZero,
			"expected_page_balance": -running, // payable is negative-tracked
		},
	})
}

// handleTokenDrill surfaces the token-dimension analytics (utm_source /
// subid / placement performance) — built in v0.2.0, previously invisible.
// Query: ?dim=utm_source&from=20260801&to=20260831&limit=50
func (a *api) handleTokenDrill(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	dim := r.URL.Query().Get("dim")
	if dim != "" && dim != "utm_source" && dim != "subid" && dim != "placement" {
		apiError(w, http.StatusBadRequest, "dim must be utm_source|subid|placement|empty")
		return
	}
	from, to := int64(0), int64(99991231)
	if s := r.URL.Query().Get("from"); s != "" {
		if v, e := strconv.ParseInt(s, 10, 64); e == nil {
			from = v
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		if v, e := strconv.ParseInt(s, 10, 64); e == nil {
			to = v
		}
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, e := strconv.Atoi(s); e == nil && v > 0 && v <= 500 {
			limit = v
		}
	}
	if a.stats == nil {
		apiError(w, http.StatusServiceUnavailable, "stats not configured")
		return
	}
	rows, err := a.stats.Drill(r.Context(), affID, dim, from, to, settings.Currency(r.Context(), a.db), limit)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "drill failed")
		return
	}
	if rows == nil {
		rows = []analytics.DrillRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// handleUniques surfaces uniq_daily — true unique-visitor counts per day
// (hyper-log-log-style exact counters the committer maintains). Built,
// tested, never readable until now.
func (a *api) handleUniques(w http.ResponseWriter, r *http.Request, idn *apikeys.Identity) {
	affID, err := targetAffiliateID(r, idn)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	days := 30
	if s := r.URL.Query().Get("days"); s != "" {
		if v, e := strconv.Atoi(s); e == nil && v > 0 && v <= 366 {
			days = v
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT day, COUNT(*) FROM uniq_daily
		WHERE affiliate_id = ? AND day >= ?
		GROUP BY day ORDER BY day DESC`,
		affID, dayInt(time.Now().UTC().AddDate(0, 0, -days)))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "uniques query failed")
		return
	}
	defer rows.Close()
	type row struct {
		Day     int64 `json:"day"`
		Uniques int64 `json:"uniques"`
	}
	out := []row{}
	for rows.Next() {
		var rr row
		if err := rows.Scan(&rr.Day, &rr.Uniques); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": out, "window_days": days})
}

func dayInt(t time.Time) int64 {
	return int64(t.Year())*10000 + int64(t.Month())*100 + int64(t.Day())
}

// --- admin endpoints ---

// currencyStatusTotals is one currency's commission totals by status.
type currencyStatusTotals struct {
	Currency      string           `json:"currency"`
	ByStatusCents map[string]int64 `json:"by_status_cents"`
}

func (a *api) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := a.db
	progCur := settings.Currency(ctx, db)

	// Commission totals by currency and status: never summed across
	// currencies.
	byCur := map[string]map[string]int64{}
	rows, err := db.QueryContext(ctx, `
		SELECT currency, status, COALESCE(SUM(amount),0) FROM commissions GROUP BY currency, status`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	for rows.Next() {
		var c, s string
		var v int64
		if rows.Scan(&c, &s, &v) == nil {
			c = money.Normalize(c)
			if byCur[c] == nil {
				byCur[c] = map[string]int64{}
			}
			byCur[c][s] += v
		}
	}
	_ = rows.Close()
	primary := primaryCurrency(byCur, progCur)
	commTotals := byCur[primary]
	if commTotals == nil {
		commTotals = map[string]int64{}
	}
	commByCur := make([]currencyStatusTotals, 0, len(byCur))
	for _, c := range sortedCurrencies(byCur) {
		commByCur = append(commByCur, currencyStatusTotals{Currency: c, ByStatusCents: byCur[c]})
	}

	var clickTotal, convTotal int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0) FROM daily_agg`).
		Scan(&clickTotal, &convTotal); err != nil {
		apiError(w, http.StatusInternalServerError, "agg query failed")
		return
	}

	paid, err := sumsByCurrency(ctx, db,
		`SELECT currency, COALESCE(SUM(amount),0) FROM payouts WHERE status='paid' GROUP BY currency`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "payout query failed")
		return
	}
	var payoutPendingCount int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM payouts WHERE status IN ('requested','approved','processing')`).
		Scan(&payoutPendingCount); err != nil {
		apiError(w, http.StatusInternalServerError, "payout query failed")
		return
	}

	var openFraud, heldCommissions int64
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fraud_verdicts WHERE resolved=FALSE`).Scan(&openFraud)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM commission_holds WHERE resolved_at IS NULL`).Scan(&heldCommissions)

	// Integrity per currency: each must be 0 (a global zero could hide a
	// +X / -X pair in two currencies).
	integrity, err := ledger.CurrencyIntegrity(ctx, db)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "integrity query failed")
		return
	}
	var ledgerSum int64
	for _, v := range integrity {
		if v < 0 {
			v = -v
		}
		ledgerSum += v // 0 iff every currency balances
	}

	// Top affiliates by lifetime earnings (paid + approved + pending) in
	// the primary currency; each lists its earnings in every currency.
	type leader struct {
		ID     int64         `json:"affiliate_id"`
		Email  string        `json:"email"`
		Cents  int64         `json:"earned_cents"` // primary currency
		Earned []money.Total `json:"earned"`       // per currency
		Status string        `json:"status"`
	}
	leaders := []leader{}
	rows, err = db.QueryContext(ctx, `
		SELECT a.id, a.email, a.status,
		       COALESCE(SUM(CASE WHEN c.currency = ? THEN c.amount ELSE 0 END),0) AS earned
		FROM affiliates a JOIN commissions c ON c.affiliate_id = a.id
		GROUP BY a.id, a.email, a.status ORDER BY earned DESC, a.id LIMIT 10`, primary)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "leaderboard query failed")
		return
	}
	for rows.Next() {
		var l leader
		if rows.Scan(&l.ID, &l.Email, &l.Status, &l.Cents) == nil {
			leaders = append(leaders, l)
		}
	}
	_ = rows.Close()
	for i := range leaders {
		e, err := sumsByCurrency(ctx, db, `
			SELECT currency, COALESCE(SUM(amount),0) FROM commissions
			WHERE affiliate_id = ? GROUP BY currency`, leaders[i].ID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "leaderboard query failed")
			return
		}
		leaders[i].Earned = totalsList(e)
	}

	daily, err := dailySeries(ctx, db, time.Now().UTC(), 30, 0, primary)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "daily query failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"daily":             daily,
		"clicks_total":      clickTotal,
		"conversions_total": convTotal,
		// The single-number money fields are in "currency" (the program
		// currency, or the one currency the program has money in); the
		// *_by_currency lists carry every currency.
		"currency":                     primary,
		"commissions_by_status_cents":  commTotals,
		"commissions_by_currency":      commByCur,
		"payouts_paid_cents":           paid[primary],
		"payouts_paid_by_currency":     totalsList(paid),
		"payouts_open_count":           payoutPendingCount,
		"fraud_open_verdicts":          openFraud,
		"commission_holds":             heldCommissions,
		"ledger_integrity_sum":         ledgerSum, // must be 0
		"ledger_integrity_by_currency": integrity.List(),
		"top_affiliates":               leaders,
	})
}

// DayStat is one UTC day of program activity from daily_agg: clicks and
// sales (every currency), commission in the series currency, and the
// day's commission in each currency (never added up).
type DayStat struct {
	Day             string        `json:"day"` // YYYY-MM-DD, UTC
	Clicks          int64         `json:"clicks"`
	Conversions     int64         `json:"conversions"`
	CommissionCents int64         `json:"commission_cents"` // minor units of the series currency
	Commissions     []money.Total `json:"commissions"`      // per currency
}

// dailySeries returns the last n UTC days ending today, oldest first, with
// days that had no activity filled in as zeros (so a chart has no gaps).
// affID > 0 limits it to one partner; 0 is the whole program. Money comes
// from daily_agg's per-currency rows: CommissionCents is currency's.
func dailySeries(ctx context.Context, db *sql.DB, now time.Time, n int, affID int64, currency string) ([]DayStat, error) {
	first := now.AddDate(0, 0, -(n - 1))
	from, _ := strconv.ParseInt(first.Format("20060102"), 10, 64)
	rows, err := db.QueryContext(ctx, `
		SELECT day, currency, COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0), COALESCE(SUM(commission_cents),0)
		FROM daily_agg WHERE day >= ? AND (? = 0 OR affiliate_id = ?) GROUP BY day, currency`, from, affID, affID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[string]DayStat{}
	perCur := map[string]money.Totals{}
	for rows.Next() {
		var d, clicks, convs, cents int64
		var cur string
		if err := rows.Scan(&d, &cur, &clicks, &convs, &cents); err != nil {
			return nil, err
		}
		k := fmt.Sprintf("%04d-%02d-%02d", d/10000, d/100%100, d%100)
		s := byDay[k]
		s.Clicks += clicks
		s.Conversions += convs
		if cur != "" {
			if cur == currency {
				s.CommissionCents += cents
			}
			if perCur[k] == nil {
				perCur[k] = money.Totals{}
			}
			perCur[k].Add(cur, cents)
		}
		byDay[k] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DayStat, 0, n)
	for i := 0; i < n; i++ {
		t := first.AddDate(0, 0, i)
		k := t.Format("2006-01-02")
		s := byDay[k]
		s.Day = k
		s.Commissions = perCur[k].List()
		out = append(out, s)
	}
	return out, nil
}

// handleAffiliates: GET /admin/affiliates?limit=&after_id= (keyset by id).
func (a *api) handleAffiliates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := a.db
	limit, afterID, err := pageParams(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	progCur := settings.Currency(ctx, db)

	type aff struct {
		ID            int64  `json:"id"`
		Email         string `json:"email"`
		Name          string `json:"name"`
		Status        string `json:"status"`
		PendingCents  int64  `json:"pending_cents"`  // program currency
		ApprovedCents int64  `json:"approved_cents"` // program currency
		PaidCents     int64  `json:"paid_cents"`     // program currency
		// Every currency the partner has money in (never summed).
		Currencies   []currencyStatusTotals `json:"currencies"`
		Clicks30d    int64                  `json:"clicks_30d"`
		PayTo        string                 `json:"pay_to"`                    // where they asked to be paid ("" = not given)
		PayToAskedAt string                 `json:"pay_to_asked_at,omitempty"` // when we last emailed them to add it
		OwnRate      *OwnRate               `json:"own_rate"`                  // their own rate; null = the program rate
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.email, a.name, a.status,
		       COALESCE(SUM(CASE WHEN c.status='pending' AND c.currency = ? THEN c.amount END),0),
		       COALESCE(SUM(CASE WHEN c.status='approved' AND c.currency = ? THEN c.amount END),0),
		       COALESCE(SUM(CASE WHEN c.status='paid' AND c.currency = ? THEN c.amount END),0),
		       (SELECT COALESCE(SUM(clicks),0) FROM daily_agg d
		        WHERE d.affiliate_id = a.id AND d.day >= ?)
		FROM affiliates a LEFT JOIN commissions c ON c.affiliate_id = a.id
		WHERE a.id > ?
		GROUP BY a.id ORDER BY a.id LIMIT ?`,
		progCur, progCur, progCur,
		time.Now().UTC().AddDate(0, 0, -30).Format("20060102"), afterID, limit)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	var out []aff = []aff{}
	for rows.Next() {
		var x aff
		if err := rows.Scan(&x.ID, &x.Email, &x.Name, &x.Status,
			&x.PendingCents, &x.ApprovedCents, &x.PaidCents, &x.Clicks30d); err != nil {
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		apiError(w, http.StatusInternalServerError, "query iteration failed")
		return
	}
	ids := make([]int64, 0, len(out))
	for _, x := range out {
		ids = append(ids, x.ID)
	}
	details := a.payToByAffiliate(ctx, ids)
	asked := partnermail.PayoutDetailsAsked(ctx, a.db, ids)
	rates := a.ownRates(ctx)
	perCur, err := a.moneyByAffiliate(ctx, ids)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	for i := range out {
		out[i].OwnRate = rates[out[i].ID]
		out[i].PayTo = details[out[i].ID].PayTo()
		if out[i].PayTo == "" {
			out[i].PayToAskedAt = asked[out[i].ID]
		}
		out[i].Currencies = perCur[out[i].ID]
		if out[i].Currencies == nil {
			out[i].Currencies = []currencyStatusTotals{}
		}
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"affiliates": out, "next_after_id": next, "currency": progCur})
}

func (a *api) handleFraudQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := a.db

	type verdict struct {
		ID        int64  `json:"id"`
		Rule      string `json:"rule"`
		Subject   string `json:"subject"`
		Reason    string `json:"reason"`
		Action    int    `json:"action"`
		DryRun    bool   `json:"dry_run"`
		CreatedAt string `json:"created_at"`
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, rule, subject, reason, action, dry_run, created_at
		FROM fraud_verdicts WHERE resolved = FALSE ORDER BY id DESC LIMIT 200`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	var verdicts []verdict = []verdict{}
	for rows.Next() {
		var v verdict
		var dry int
		if err := rows.Scan(&v.ID, &v.Rule, &v.Subject, &v.Reason, &v.Action, &dry, &v.CreatedAt); err != nil {
			_ = rows.Close()
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		v.DryRun = dry == 1
		verdicts = append(verdicts, v)
	}
	_ = rows.Close()

	type hold struct {
		CommissionID int64  `json:"commission_id"`
		AffiliateID  int64  `json:"affiliate_id"`
		Email        string `json:"email"`
		AmountCents  int64  `json:"amount_cents"`
		Reason       string `json:"reason"`
	}
	rows, err = db.QueryContext(ctx, `
		SELECT ch.commission_id, c.affiliate_id, a.email, c.amount, ch.reason
		FROM commission_holds ch
		JOIN commissions c ON c.id = ch.commission_id
		JOIN affiliates a ON a.id = c.affiliate_id
		WHERE ch.resolved_at IS NULL ORDER BY ch.commission_id DESC LIMIT 200`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "holds query failed")
		return
	}
	var holds []hold = []hold{}
	for rows.Next() {
		var h hold
		if err := rows.Scan(&h.CommissionID, &h.AffiliateID, &h.Email, &h.AmountCents, &h.Reason); err != nil {
			_ = rows.Close()
			apiError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		holds = append(holds, h)
	}
	_ = rows.Close()

	writeJSON(w, http.StatusOK, map[string]any{"verdicts": verdicts, "holds": holds})
}

// handleFraudResolve closes a review case. Body:
// {"commission_id": N, "decision": "release" | "uphold", "note": "..."}
//
//	release: mark the hold resolved; the commission re-enters payout flow.
//	uphold:  resolve the hold AND cancel the commission with a ledger
//	         clawback (expense reversal) — the money movement is undone.
func (a *api) handleFraudResolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := a.db
	var req struct {
		CommissionID int64  `json:"commission_id"`
		Decision     string `json:"decision"`
		Note         string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.CommissionID <= 0 || (req.Decision != "release" && req.Decision != "uphold") {
		apiError(w, http.StatusBadRequest, "commission_id and decision (release|uphold) required")
		return
	}

	// The open hold must exist. Distinguish already-resolved (409) from
	// never-existed (404).
	var holdID int64
	err := db.QueryRowContext(ctx, `
		SELECT ch.commission_id FROM commission_holds ch
		WHERE ch.commission_id = ? AND ch.resolved_at IS NULL`, req.CommissionID).Scan(&holdID)
	if err == sql.ErrNoRows {
		var exists int
		if e := db.QueryRowContext(ctx,
			`SELECT 1 FROM commission_holds WHERE commission_id = ?`, req.CommissionID).Scan(&exists); e == nil {
			apiError(w, http.StatusConflict, "hold already resolved")
		} else {
			apiError(w, http.StatusNotFound, "no hold for commission")
		}
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "hold lookup failed")
		return
	}

	if req.Decision == "release" {
		res, err := db.ExecContext(ctx, `
			UPDATE commission_holds SET resolved_at = ?, resolution = 'released',
			       note = COALESCE(NULLIF(?, ''), note)
			WHERE commission_id = ? AND resolved_at IS NULL`,
			time.Now().UTC().Format(time.RFC3339), req.Note, req.CommissionID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "release failed")
			return
		}
		if n, _ := res.RowsAffected(); n != 1 {
			apiError(w, http.StatusConflict, "hold already resolved")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"commission_id": req.CommissionID, "decision": "released"})
		return
	}

	// uphold: the ONE production path — fraud.Uphold (shared with workers).
	// The claw journal, the cancel and the hold resolution commit in one
	// ledger transaction under the commission's row lock.
	if a.deps.LedgerPoster == nil {
		apiError(w, http.StatusNotImplemented, "uphold requires a ledger poster")
		return
	}
	// Clawback = what is still live (amount minus earlier partial refunds),
	// matching what fraud.Uphold actually claws.
	var amount int64
	if err := db.QueryRowContext(ctx, `
		SELECT c.amount + COALESCE((SELECT SUM(k.amount) FROM commissions k
		                            WHERE k.parent_commission_id = c.id AND k.amount < 0), 0)
		FROM commissions c WHERE c.id = ?`, req.CommissionID).Scan(&amount); err != nil {
		apiError(w, http.StatusInternalServerError, "commission lookup failed")
		return
	}
	err = fraud.Uphold(ctx, db, a.deps.LedgerPoster, req.CommissionID, req.Note)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"commission_id": req.CommissionID, "decision": "upheld", "clawback_cents": amount})
	case errors.Is(err, fraud.ErrAlreadyPaid):
		apiError(w, http.StatusConflict, "commission already paid; use refund flow")
	case errors.Is(err, fraud.ErrTerminalState):
		apiError(w, http.StatusConflict, "commission in terminal state")
	case errors.Is(err, fraud.ErrPayoutInFlight):
		apiError(w, http.StatusConflict, "commission is in a payout being executed; retry after it settles")
	default:
		apiError(w, http.StatusInternalServerError, "uphold failed: "+err.Error())
	}
}

// handlePayoutApprove transitions a requested payout through processing to
// paid (admin): executes the configured rail (nil = manual), posts the
// ledger settlement, marks member commissions paid. Body: {"payout_id": N}.
func (a *api) handlePayoutApprove(w http.ResponseWriter, r *http.Request) {
	if a.deps.PayoutRunner == nil {
		apiError(w, http.StatusServiceUnavailable, "payout runner not configured")
		return
	}
	var body struct {
		PayoutID int64 `json:"payout_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if body.PayoutID <= 0 {
		apiError(w, http.StatusBadRequest, "payout_id required")
		return
	}
	err := a.deps.PayoutRunner.Approve(r.Context(), body.PayoutID, a.deps.PayoutExecutor)
	if errors.Is(err, payouts.ErrAlreadyPaid) {
		// Idempotent: a double click or retried request after success
		// answers 200 and moves no money.
		writeJSON(w, http.StatusOK, map[string]any{"payout_id": body.PayoutID, "status": "paid", "already_paid": true})
		return
	}
	if err != nil {
		apiError(w, http.StatusConflict, "approve failed: "+err.Error())
		return
	}
	emailed := false
	if a.deps.MailOn {
		// After the money moved: a failed enqueue costs an email, never a payout.
		if err := partnermail.EnqueuePayoutPaid(r.Context(), a.db, body.PayoutID); err != nil {
			log.Printf("affiliate-engine: payout %d: queue paid email: %v", body.PayoutID, err)
		} else {
			emailed = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payout_id": body.PayoutID, "status": "paid", "emailed": emailed})
}

// handlePayoutRun triggers a payout batch (admin). Body:
// {"min_threshold_cents": N, "method": "manual"|"paypal"|"wise", "dry_run": bool}
//
// min_threshold_cents omitted (or null) = the program minimum
// (program_settings.min_payout_cents, default 5000). An explicit value
// overrides it for this run only.
func (a *api) handlePayoutRun(w http.ResponseWriter, r *http.Request) {
	if a.deps.PayoutRunner == nil {
		apiError(w, http.StatusNotImplemented, "payout runner not configured")
		return
	}
	var body struct {
		MinThresholdCents *int64 `json:"min_threshold_cents"`
		Method            string `json:"method"`
		DryRun            bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if body.Method == "" {
		body.Method = "manual"
	}
	ctx := r.Context()
	// One explicit min_threshold_cents applies to every currency (minor
	// units of each, as before); the program settings carry a minimum per
	// currency (min_payout_by_currency, else min_payout_cents).
	var threshold int64
	var th payouts.Thresholds
	if body.MinThresholdCents != nil {
		if *body.MinThresholdCents < 0 {
			apiError(w, http.StatusBadRequest, "min_threshold_cents must be >= 0")
			return
		}
		threshold = *body.MinThresholdCents
		th = payouts.Uniform(threshold)
	} else {
		ps, err := settings.Load(ctx, a.db)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "settings unavailable")
			return
		}
		threshold = ps.MinPayoutCents
		th = payouts.Thresholds{Default: ps.MinPayoutCents, ByCurrency: ps.MinPayoutByCurrency}
	}

	if body.DryRun {
		// Preview only: the exact plan CreateBatch would write (same
		// selection code: tax-doc gate, holds, clawback netting, one
		// payout per affiliate+currency), nothing written.
		out, err := a.deps.PayoutRunner.PreviewWith(ctx, th)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "dry-run failed")
			return
		}
		if out == nil {
			out = []payouts.PlannedPayout{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "min_threshold_cents": threshold, "min_threshold_by_currency": th.ByCurrency, "batches": out})
		return
	}

	runID, n, err := a.deps.PayoutRunner.CreateBatchWith(ctx, th, body.Method)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "payout run failed: "+err.Error())
		return
	}
	if err := a.deps.PayoutRunner.CompleteRun(ctx, runID,
		fmt.Sprintf("api: %d payouts, method=%s", n, body.Method)); err != nil {
		apiError(w, http.StatusInternalServerError, "complete run failed")
		return
	}
	// Partners in this run who haven't said where to be paid get an email
	// now (the engine's ticker would, within half an hour).
	asked := 0
	if a.deps.MailOn && n > 0 {
		if asked, err = partnermail.AskPayoutDetails(ctx, a.db, time.Now()); err != nil {
			log.Printf("payout run: asking for payout details: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "payouts_created": n, "min_threshold_cents": threshold, "min_threshold_by_currency": th.ByCurrency, "asked_for_details": asked})
}

// --- helpers ---

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte("rl:" + key)) // rate-limit bucket key (not the stored hash)
	return hex.EncodeToString(sum[:8])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- rate limiter: fixed window per key, constant space ---

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window int64            // unix minutes
	seen   map[string]int64 // bucket key -> unix minute
	counts map[string]int64 // bucket key -> count in window
	calls  uint64           // lazy-sweep counter
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = 600
	}
	return &rateLimiter{
		limit:  perMinute,
		window: -1,
		seen:   map[string]int64{},
		counts: map[string]int64{},
	}
}

func (rl *rateLimiter) allow(bucket string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now().Unix() / 60
	if rl.seen[bucket] != now {
		// Window rolled: recycle the entry (constant space: one slot per key).
		rl.seen[bucket] = now
		rl.counts[bucket] = 0
	}
	rl.counts[bucket]++
	allow := rl.counts[bucket] <= int64(rl.limit)

	// Bounded memory (audit P1-4): unauthenticated buckets (pub:<ip>) give
	// an IPv6 rotator unlimited distinct keys — slow remote memory
	// exhaustion. Lazy sweep every 4096 calls evicts entries whose window
	// rolled and stayed cold for 2+ minutes (we keep the just-recycled
	// current key by only evicting seen < now-1).
	rl.calls++
	if rl.calls&4095 == 0 {
		for k, m := range rl.seen {
			if m < now-1 {
				delete(rl.seen, k)
				delete(rl.counts, k)
			}
		}
	}
	return allow
}

// exceeded reports, without counting, whether bucket has already used its
// budget in the current window (used to refuse before doing any work).
func (rl *rateLimiter) exceeded(bucket string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.seen[bucket] == time.Now().Unix()/60 && rl.counts[bucket] >= int64(rl.limit)
}
