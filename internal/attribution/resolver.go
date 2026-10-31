// Package attribution resolves which affiliate earns a conversion, using the
// server-side ladder: coupon → customer binding → signed token → IP fallback
// (flagged heuristic of last resort). The cookie is one channel, never the
// authority (FINAL-PLAN.md §4).
//
// Only ACTIVE affiliates earn: every rung joins affiliates and requires
// status = 'active'. A frozen or removed affiliate's coupon, customer
// bindings, tokens and clicks resolve to unattributed — freezing an
// account must stop new commissions, not just new clicks.
package attribution

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/tokens"
)

// Resolution is the outcome of the ladder.
type Resolution struct {
	AffiliateID int64
	CampaignID  int64
	Method      string // "coupon" | "customer" | "token" | "ip_fallback" | ""
	ClickID     string // when method == token
	Flagged     bool   // true for ip_fallback (heuristic, reviewable)
}

// Resolver runs the ladder against the store.
type Resolver struct {
	db     *sql.DB
	signer *tokens.Signer
}

// NewResolver binds a resolver.
func NewResolver(db *sql.DB, signer *tokens.Signer) *Resolver {
	return &Resolver{db: db, signer: signer}
}

// Query is the input to resolution.
type Query struct {
	CouponCode    string
	CustomerExtID string
	Token         string // signed attribution token from cookie/metadata
	IPHash        []byte
	CampaignID    int64
	Now           time.Time
}

// Resolve walks the ladder in fixed precedence order.
// Returns a zero Resolution (Method == "") when unattributed.
//
// A rung that matches a NON-active affiliate ends the ladder as
// unattributed rather than falling through: the buyer's strongest signal
// points at a frozen account, and handing the sale to a weaker signal
// (another affiliate's stale cookie) would be a mis-attribution.
func (r *Resolver) Resolve(ctx context.Context, q Query) (Resolution, error) {
	// 1. Coupon: strongest signal — buyer deliberately used a code.
	if q.CouponCode != "" {
		var aff, camp int64
		var status string
		err := r.db.QueryRowContext(ctx, `
			SELECT c.affiliate_id, c.campaign_id, a.status FROM coupons c
			JOIN affiliates a ON a.id = c.affiliate_id
			WHERE c.code = ? AND c.active = TRUE`, q.CouponCode).Scan(&aff, &camp, &status)
		if err == nil {
			if status != "active" {
				return Resolution{}, nil
			}
			return Resolution{AffiliateID: aff, CampaignID: camp, Method: "coupon"}, nil
		}
		if err != sql.ErrNoRows {
			return Resolution{}, fmt.Errorf("attribution: coupon: %w", err)
		}
	}

	// 2. Customer binding: lifetime, immutable, first-touch. It credits in
	// the program the customer was bound under.
	if q.CustomerExtID != "" {
		var aff, camp int64
		var status string
		err := r.db.QueryRowContext(ctx, `
			SELECT cu.affiliate_id, cu.campaign_id, a.status FROM customers cu
			JOIN affiliates a ON a.id = cu.affiliate_id
			WHERE cu.program_id = 1 AND cu.external_customer_id = ?`, q.CustomerExtID).Scan(&aff, &camp, &status)
		if err == nil {
			if status != "active" {
				return Resolution{}, nil
			}
			return Resolution{AffiliateID: aff, CampaignID: orCampaign(camp, q.CampaignID), Method: "customer"}, nil
		}
		if err != sql.ErrNoRows {
			return Resolution{}, fmt.Errorf("attribution: customer: %w", err)
		}
	}

	// 3. Signed token: click within the attribution window.
	if q.Token != "" && r.signer != nil {
		payload, err := r.signer.Parse(q.Token)
		if err == nil {
			// Window check: the token carries exp; the attribution row is the
			// server-side record — verify a non-expired attribution exists.
			// The affiliate must be active NOW, not merely at click time.
			// The click's program (the link's campaign) is the sale's program.
			var count, camp int64
			var status string
			err := r.db.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(MAX(campaign_id), 0) FROM attributions
				WHERE click_id = ? AND expires_at > ?`,
				payload.ClickID, q.Now.UTC().Format(time.RFC3339)).Scan(&count, &camp)
			if err == nil && count > 0 {
				serr := r.db.QueryRowContext(ctx,
					`SELECT status FROM affiliates WHERE id = ?`, payload.AffiliateID).Scan(&status)
				switch {
				case serr == nil && status == "active":
					return Resolution{
						AffiliateID: payload.AffiliateID,
						CampaignID:  orCampaign(camp, q.CampaignID),
						Method:      "token",
						ClickID:     payload.ClickID,
					}, nil
				case serr == nil, serr == sql.ErrNoRows:
					return Resolution{}, nil // frozen/removed/unknown affiliate
				default:
					return Resolution{}, fmt.Errorf("attribution: token affiliate: %w", serr)
				}
			}
		}
		// Invalid/expired token: fall through (never error the conversion).
	}

	// 4. IP fallback: flagged heuristic — latest click from this identity
	// within 24h, from an affiliate that is still active. Recorded as
	// flagged for fraud review, never silent truth.
	if q.IPHash != nil {
		var aff, camp int64
		err := r.db.QueryRowContext(ctx, `
			SELECT c.affiliate_id, c.campaign_id FROM clicks c
			JOIN affiliates a ON a.id = c.affiliate_id
			WHERE c.ip_hash = ? AND c.exclude_stats = 0
			  AND c.ts > ? AND a.status = 'active'
			ORDER BY c.ts DESC LIMIT 1`,
			q.IPHash, q.Now.UTC().Add(-24*time.Hour).Format(time.RFC3339Nano)).Scan(&aff, &camp)
		if err == nil {
			return Resolution{
				AffiliateID: aff,
				CampaignID:  orCampaign(camp, q.CampaignID),
				Method:      "ip_fallback",
				Flagged:     true,
			}, nil
		}
		if err != sql.ErrNoRows {
			return Resolution{}, fmt.Errorf("attribution: ip fallback: %w", err)
		}
	}

	return Resolution{}, nil
}

// orCampaign is the stored program when there is one, else the caller's
// default.
func orCampaign(stored, fallback int64) int64 {
	if stored > 0 {
		return stored
	}
	return fallback
}
