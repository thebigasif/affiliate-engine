package main

// runTestConversion: fire a signed test conversion through the REAL money
// path on a local database — the M1 acceptance drill ("first tracked test
// commission < 15 minutes on a fresh install") without needing Stripe
// secrets. Uses the commissions engine end to end: raw_events idempotency,
// attribution resolution (explicit affiliate id), rate resolution from
// commission_rules, ledger posting, commission row.
//
//	affiliate-engine test-conversion -affiliate 1 -amount 2000
//
// The event source is "test" — visible in raw_events/commissions for audit;
// it is a real commission (creditable, maturable, payable) so operators can
// exercise the full lifecycle. Replays are rejected exactly like production.
import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/commissions"
	"github.com/thebigasif/affiliate-engine/internal/fraud"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
	"github.com/thebigasif/affiliate-engine/internal/mlm"
	"github.com/thebigasif/affiliate-engine/internal/screening"
)

func runTestConversion(dbPath string, args []string) error {
	fs := flag.NewFlagSet("test-conversion", flag.ContinueOnError)
	affID := fs.Int64("affiliate", 0, "affiliate id (required)")
	amount := fs.Int64("amount", 2000, "gross commissionable amount in minor units (cents)")
	extID := fs.String("external-id", "", "idempotency id (default: test-<random>)")
	customer := fs.String("customer", "", "external customer id (sets the lifetime binding)")
	campaign := fs.Int64("campaign", 1, "campaign id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *affID <= 0 {
		return fmt.Errorf("test-conversion: -affiliate is required (see /api/v1/admin/affiliates)")
	}
	if *amount <= 0 {
		return fmt.Errorf("test-conversion: -amount must be positive minor units")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := openForCLI(ctx, dbPath)
	if err != nil {
		return err
	}
	defer h.DB.Close()

	// Pre-flight: affiliate + a matching rule must exist so the failure is
	// actionable (the operator learns what to create next, not a 500).
	var affCount int
	if err := h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM affiliates WHERE id = ?`, *affID).Scan(&affCount); err != nil {
		return err
	}
	if affCount == 0 {
		return fmt.Errorf("test-conversion: affiliate %d not found — create one first (portal apply + admin review)", *affID)
	}
	var ruleCount int
	if err := h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM commission_rules WHERE campaign_id = ? AND active = TRUE`, *campaign).Scan(&ruleCount); err != nil {
		return err
	}
	if ruleCount == 0 {
		return fmt.Errorf("test-conversion: no active commission rule for campaign %d — add one: POST /api/v1/admin/rules {\"campaign_id\":%d,\"rate_type\":\"percent\",\"rate_value\":1000}", *campaign, *campaign)
	}

	id := *extID
	if id == "" {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return err
		}
		id = fmt.Sprintf("test-%x", buf[:])
	}

	poster := ledger.NewPoster(h.Ledger)
	rawEngine := commissions.NewEngine(h.DB, poster)
	rawEngine.MLMBV = mlm.New(h.DB).RecordBV // trees see drill volume too
	// Screen test conversions through the same fraud path production
	// webhooks use, so the drill is representative of live traffic.
	engine := screening.New(rawEngine, fraud.NewEngine(h.DB), false)
	cid, err := engine.ProcessPurchase(ctx, commissions.Event{
		Source:        "test",
		ExternalID:    id,
		EventType:     "purchase",
		AmountMinor:   *amount,
		Currency:      "USD",
		AffiliateID:   *affID,
		CampaignID:    *campaign,
		CustomerExtID: *customer,
		OccurredAt:    time.Now().UTC(),
	})
	if err != nil {
		if errors.Is(err, commissions.ErrAlreadyProcessed) {
			return fmt.Errorf("test-conversion: external-id %q already processed (replays are rejected, exactly like production)", id)
		}
		return fmt.Errorf("test-conversion: %w", err)
	}

	var amountOut int64
	var status string
	if err := h.DB.QueryRowContext(ctx, `SELECT amount, status FROM commissions WHERE id = ?`, cid).Scan(&amountOut, &status); err != nil {
		return fmt.Errorf("test-conversion: read back commission #%d: %w", cid, err)
	}
	fmt.Printf("commission #%d created: status=%s amount=%.2f USD (source=test, external_id=%s)\n",
		cid, status, float64(amountOut)/100, id)
	fmt.Println("verify: ./affiliate-engine -db <db> doctor   # all checks must pass")
	return nil
}
