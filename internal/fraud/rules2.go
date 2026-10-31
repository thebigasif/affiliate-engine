package fraud

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// ConversionTimeAnomaly flags conversions whose hour-of-day distribution
// diverges wildly from the program baseline (threat model rule 7: bot farms
// never sleep; human traffic follows diurnal curves). Statistical v1:
// compare the affiliate's conversion-hour variance against the program's
// using a z-score on the night-share (00:00-05:00 local) proportion.
type ConversionTimeAnomaly struct{ DB *sql.DB }

func (ConversionTimeAnomaly) Name() string { return "conversion_time_anomaly" }

// Evaluate: affiliate flagged when (night conversions / total) exceeds the
// program baseline + margin with a minimum sample.
// created_at is TEXT on SQLite and timestamptz on Postgres; as text both
// put the UTC hour at characters 12-13 ("2026-09-30T15:..." and
// "2026-09-30 15:...+00", the Postgres session being pinned to UTC).
func (r ConversionTimeAnomaly) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	const minSample = 20
	var night, total int64
	err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN CAST(SUBSTR(CAST(created_at AS TEXT), 12, 2) AS INTEGER) < 6 THEN 1 ELSE 0 END), 0),
		  COUNT(*)
		FROM commissions
		WHERE affiliate_id = ? AND created_at > ?`,
		q.AffiliateID, q.Now.UTC().Add(-7*24*time.Hour).Format(time.RFC3339)).Scan(&night, &total)
	// Errors first: the old `err == ErrNoRows || total < minSample`
	// shortcut read total == 0 on ANY error and silently passed.
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == sql.ErrNoRows || total < minSample {
		return nil, nil
	}
	nightShare := float64(night) / float64(total)

	// Program baseline (all affiliates, same window).
	var pNight, pTotal int64
	if err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN CAST(SUBSTR(CAST(created_at AS TEXT), 12, 2) AS INTEGER) < 6 THEN 1 ELSE 0 END), 0),
		  COUNT(*)
		FROM commissions WHERE created_at > ?`,
		q.Now.UTC().Add(-7*24*time.Hour).Format(time.RFC3339)).Scan(&pNight, &pTotal); err != nil {
		return nil, err
	}
	if pTotal < minSample {
		return nil, nil // not enough program data to judge
	}
	pShare := float64(pNight) / float64(pTotal)

	// Two-proportion z-test (normal approx; sufficient for a flag, human
	// reviews). Flag when z > 3 (one-in-a-thousand under null).
	se := math.Sqrt(pShare*(1-pShare)/float64(total) + epsilon)
	z := (nightShare - pShare) / se
	if z > 3 && nightShare > 0.5 {
		return []Verdict{{
			Rule:    "conversion_time_anomaly",
			Subject: fmt.Sprint(q.AffiliateID),
			Reason: fmt.Sprintf("%.0f%% of %d sales came between midnight and 6am UTC, against %.0f%% across the program (z=%.1f)",
				nightShare*100, total, pShare*100, z),
			Action: ActionHold,
		}}, nil
	}
	return nil, nil
}

const epsilon = 1e-9

// ASNReputation flags affiliates whose recent clicks come predominantly
// from datacenter/hosting ranges (Methbot pattern). Counts come from
// dc_click_daily — classified at ingest (fingerprint.IsDatacenterIP), raw
// IPs never stored.
type ASNReputation struct{ DB *sql.DB }

func (ASNReputation) Name() string { return "asn_reputation" }

// Evaluate: >=80% datacenter share over the last 7 days, min 20 clicks,
// and at least minClickRuleConversions attributed conversions in the same
// window — click share alone is attacker-manufacturable.
func (r ASNReputation) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	since := q.Now.UTC().AddDate(0, 0, -7)
	var dc, total int64
	err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT COALESCE(SUM(dc_clicks),0), COALESCE(SUM(total_clicks),0)
		FROM dc_click_daily
		WHERE affiliate_id = ? AND day >= ?`,
		q.AffiliateID, since.Format("20060102")).Scan(&dc, &total)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == sql.ErrNoRows || total < 20 {
		return nil, nil
	}
	if float64(dc)/float64(total) < 0.8 {
		return nil, nil
	}
	ok, err := enoughConversions(ctx, Conn(ctx, r.DB), q.AffiliateID, since)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return []Verdict{{
		Rule:    "asn_reputation",
		Subject: fmt.Sprint(q.AffiliateID),
		Reason:  fmt.Sprintf("%d of %d clicks in the last 7 days came from data centers, not homes or phones", dc, total),
		Action:  ActionHold,
	}}, nil
}
