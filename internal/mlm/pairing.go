package mlm

import (
	"context"
	"fmt"

	"github.com/thebigasif/affiliate-engine/internal/bpmath"
)

// Pairing pays binary-plan bonuses on completed pairs: an affiliate earns
// when BOTH outer legs contain volume. Classic rules (clean-room from the
// guruperl failure study, fixed: no float, no silently-consumed BV):
//   - a "pair" = 1 paid unit on the LEFT outer leg + 1 on the RIGHT
//   - payout per pair is fixed minor units (plan config)
//   - matched volume is tracked per (day, affiliate) and NEVER consumed
//     silently: weaker-leg units deduct, remainder carries forward
//   - exactly-once per (period, affiliate, pair_seq) via unique key

// PairConfig is the plan definition.
type PairConfig struct {
	PairPayoutCents int64 // fixed payout per completed pair
	MaxPairsPerDay  int64 // daily cap on pairs PAID per (affiliate, day) (0 = uncapped)
}

// PairVolume records matched-pair state per affiliate (materialized on write).
type PairVolume struct {
	AffiliateID int64
	Day         int64
	LegL        int64 // cumulative LEFT-leg volume units
	LegR        int64 // cumulative RIGHT-leg volume units
	PairsPaid   int64 // pairs already paid (consumed volume = 2x this)
}

// RecordLegVolume folds one volume event into the affiliate's legs. The
// OUTER leg attribution uses the placement closure (the leg through which
// the descendant hangs), so upline pair counting is a closure query.
func (e *Engine) RecordLegVolume(ctx context.Context, ancestorID int64, outerLeg string, day int64, units int64) error {
	if outerLeg != "L" && outerLeg != "R" {
		return fmt.Errorf("mlm: pairing: leg must be L or R, got %q", outerLeg)
	}
	col := "leg_l"
	if outerLeg == "R" {
		col = "leg_r"
	}
	_, err := e.db.ExecContext(ctx, `
		INSERT INTO pair_volume (affiliate_id, day, `+col+`)
		VALUES (?, ?, ?)
		ON CONFLICT(affiliate_id, day)
		DO UPDATE SET `+col+` = pair_volume.`+col+` + ?`,
		ancestorID, day, units, units)
	if err != nil {
		return fmt.Errorf("mlm: pairing: volume: %w", err)
	}
	return nil
}

// SettlePairs computes and records new pairs for one affiliate on one day.
// A new pair = min(legL, legR) - pairsPaid (the weaker leg governs).
// Returns pairs newly paid. Idempotent: pair state persists in the row.
func (e *Engine) SettlePairs(ctx context.Context, affiliateID int64, day int64, cfg PairConfig) (pairs int64, payoutCents int64, err error) {
	if cfg.PairPayoutCents < 0 {
		return 0, 0, fmt.Errorf("mlm: pairing: negative pair payout %d", cfg.PairPayoutCents)
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var legL, legR, paid int64
	if err := tx.QueryRowContext(ctx, `
		SELECT leg_l, leg_r, pairs_paid FROM pair_volume
		WHERE affiliate_id = ? AND day = ?`, affiliateID, day).
		Scan(&legL, &legR, &paid); err != nil {
		return 0, 0, err // no volume row = nothing to settle
	}

	// min leg minus already-paid pairs = new pairs.
	newPairs := legL
	if legR < newPairs {
		newPairs = legR
	}
	newPairs -= paid
	if newPairs <= 0 {
		return 0, 0, nil
	}
	// Daily cap: MaxPairsPerDay bounds the pairs PAID for the day (the row
	// is per day), not per call — the old per-call cap let every repeated
	// settle on the same day pay a fresh cap's worth. Pairs beyond the cap
	// stay unpaid for the day (standard binary-plan flush).
	if cfg.MaxPairsPerDay > 0 {
		allowed := cfg.MaxPairsPerDay - paid
		if allowed <= 0 {
			return 0, 0, nil
		}
		if newPairs > allowed {
			newPairs = allowed
		}
	}
	payout, ok := bpmath.MulDiv(newPairs, cfg.PairPayoutCents, 1)
	if !ok {
		return 0, 0, fmt.Errorf("mlm: pairing: payout overflow: pairs=%d per_pair=%d", newPairs, cfg.PairPayoutCents)
	}

	// Exactly-once marker: pairs_paid advances only on successful settle.
	// The WHERE pairs_paid = ? is a CAS: on PG READ COMMITTED a concurrent
	// winner's commit makes the loser's UPDATE re-evaluate and match ZERO
	// rows — with err still nil. RowsAffected MUST be checked or the loser
	// reports a payout that never happened (nightly 09-25, attack 2b).
	res, err := tx.ExecContext(ctx, `
		UPDATE pair_volume SET pairs_paid = pairs_paid + ?, payout_cents = payout_cents + ?
		WHERE affiliate_id = ? AND day = ? AND pairs_paid = ?`,
		newPairs, payout, affiliateID, day, paid)
	if err != nil {
		return 0, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		// CAS lost: a concurrent settle consumed these pairs. Report
		// nothing — the row already reflects the winner's payout, and a
		// caller posting from the return value would double-pay. Any
		// volume that arrived after our read is picked up by the next
		// settle (remainder carries forward, never silently consumed).
		return 0, 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return newPairs, payout, nil
}
