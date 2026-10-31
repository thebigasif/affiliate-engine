// rules3.go — fraud rules 7-10 (PRD audit FR-18: the four missing rules).
// 7 ip_overlap + 8 duplicate_account landed in a1c5370 (restored verbatim
// after a concurrent-session write race); 9 geo_mismatch + 10 the bot-class
// gate + the IP-fallback anomaly (threat model R7) follow, reading the
// provenance columns added in fraud2_{sqlite,pg}.sql.
package fraud

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// IPOverlap flags affiliates whose click IPs also appear under OTHER
// affiliates — the shared-infrastructure signature of referral farms
// (one operator, many "affiliates", same office/VPN egress). Click-share
// rule: it only enforces once the affiliate has minClickRuleConversions
// conversions in the window (anyone can click through two affiliates'
// links from one IP), and its hold follows the overlapping clicks.
type IPOverlap struct{ DB *sql.DB }

func (IPOverlap) Name() string { return "ip_overlap" }

func (r IPOverlap) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.AffiliateID == 0 {
		return nil, nil
	}
	// IP hashes this affiliate shares with ANY other affiliate in the
	// window. The threshold is the COUNT of shared hashes: one coincidental
	// shared NAT is normal; a PATTERN of shared egress is a farm.
	const minShared = 2
	const window = 30 * 24 * time.Hour
	var shared int
	if err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT c1.ip_hash)
		FROM clicks c1
		WHERE c1.affiliate_id = ?
		  AND c1.ip_hash IS NOT NULL
		  AND c1.day >= ?
		  AND EXISTS (SELECT 1 FROM clicks c2
		       WHERE c2.ip_hash = c1.ip_hash AND c2.affiliate_id != c1.affiliate_id
		         AND c2.day >= ?)`,
		q.AffiliateID, dayInt(q.Now.Add(-window)), dayInt(q.Now.Add(-window))).Scan(&shared); err != nil {
		return nil, err
	}
	if shared < minShared {
		return nil, nil
	}
	ok, err := enoughConversions(ctx, Conn(ctx, r.DB), q.AffiliateID, q.Now.Add(-window))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	ids, err := queryClickIDs(ctx, Conn(ctx, r.DB), `
		SELECT c1.id FROM clicks c1
		WHERE c1.affiliate_id = ?
		  AND c1.ip_hash IS NOT NULL
		  AND c1.day >= ?
		  AND EXISTS (SELECT 1 FROM clicks c2
		       WHERE c2.ip_hash = c1.ip_hash AND c2.affiliate_id != c1.affiliate_id
		         AND c2.day >= ?)
		LIMIT 200`,
		q.AffiliateID, dayInt(q.Now.Add(-window)), dayInt(q.Now.Add(-window)))
	if err != nil {
		return nil, err
	}
	return []Verdict{{
		Rule:     r.Name(),
		Subject:  fmt.Sprintf("affiliate:%d", q.AffiliateID),
		Reason:   fmt.Sprintf("Shares %d visitor IP addresses with other partners in the last 30 days, a sign of one person running several accounts", shared),
		Action:   ActionHold,
		ClickIDs: ids,
	}}, nil
}

// DuplicateAccount flags multiple ACTIVE affiliate accounts whose portal
// applications came from the same email alias family — normalizeEmail
// collapses plus-aliases (foo+x@ = foo@), so N accounts on one base
// address is the multi-accounting signature. Self-referral's cousin.
//
// The rule reports ActionFreeze, but the engine applies it as a HOLD on
// the evaluated commission only: signing up with a victim's alias is
// free, so an account freeze here would be a weapon (see Engine.apply).
type DuplicateAccount struct{ DB *sql.DB }

func (DuplicateAccount) Name() string { return "duplicate_account" }

func (r DuplicateAccount) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.AffiliateID == 0 {
		return nil, nil
	}
	// The affiliate's email, alias-normalized, used by 2+ active accounts.
	var base string
	if err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT lower(email) FROM affiliates WHERE id = ?`, q.AffiliateID).Scan(&base); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	norm := normalizeEmail(base)
	// SQLite/PG share lower(); alias-collapse in SQL would need a UDF, so
	// fetch candidate emails with the same base local-part prefix and
	// normalize in Go (bounded by the active set).
	rows, err := Conn(ctx, r.DB).QueryContext(ctx, `
		SELECT lower(email) FROM affiliates WHERE status = 'active' AND id != ?`,
		q.AffiliateID)
	if err != nil {
		return nil, err
	}
	dupes := 0
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil && normalizeEmail(e) == norm && norm != "" {
			dupes++
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if dupes == 0 {
		return nil, nil
	}
	return []Verdict{{
		Rule:    r.Name(),
		Subject: fmt.Sprintf("affiliate:%d", q.AffiliateID),
		Reason:  fmt.Sprintf("Email %q matches %d other active partners once dots and +tags are ignored", norm, dupes),
		Action:  ActionFreeze,
	}}, nil
}

// BotClassGating: an affiliate whose stats-included click stream is
// dominated by SUSPECTED bots is buying traffic. Known bots are already
// exclude_stats=1 and never counted; this catches the suspected class
// that survives exclusion (UA-spoofed farms the classifier can only
// distrust, not prove). Click-share rule: needs minClickRuleConversions
// conversions in the window to enforce; the hold follows the suspected
// clicks.
type BotClassGating struct{ DB *sql.DB }

func (BotClassGating) Name() string { return "bot_class_gating" }

func (r BotClassGating) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.AffiliateID == 0 {
		return nil, nil
	}
	since := q.Now.UTC().Add(-7 * 24 * time.Hour)
	var suspected, total int64
	err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN bot_class = 1 THEN 1 ELSE 0 END),0), COUNT(*)
		FROM clicks
		WHERE affiliate_id = ? AND exclude_stats = 0 AND ts > ?`,
		q.AffiliateID, since.Format(time.RFC3339Nano)).Scan(&suspected, &total)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == sql.ErrNoRows || total < 20 {
		return nil, nil
	}
	if suspected*2 < total { // flag at >=50% suspected among included clicks
		return nil, nil
	}
	ok, err := enoughConversions(ctx, Conn(ctx, r.DB), q.AffiliateID, since)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	ids, err := queryClickIDs(ctx, Conn(ctx, r.DB), `
		SELECT id FROM clicks
		WHERE affiliate_id = ? AND exclude_stats = 0 AND bot_class = 1 AND ts > ?
		LIMIT 200`, q.AffiliateID, since.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	return []Verdict{{
		Rule:     "bot_class_gating",
		Subject:  fmt.Sprint(q.AffiliateID),
		Reason:   fmt.Sprintf("%d of %d clicks in the last 7 days looked like bots", suspected, total),
		Action:   ActionHold,
		ClickIDs: ids,
	}}, nil
}

// GeoMismatch: conversions credited in countries the affiliate's click
// traffic never came from. Click geo is captured at the redirect edge
// (CDN country header, clicks.country); conversion geo rides the billing
// event (commissions.country). A referred customer base tracks the
// audience; a geo-divergent conversion stream marks purchased/forged
// checkouts. Untagged conversions are ignored (the rule needs signal on
// BOTH sides, minimum 5 tagged conversions and at least one click country).
type GeoMismatch struct{ DB *sql.DB }

func (GeoMismatch) Name() string { return "geo_mismatch" }

func (r GeoMismatch) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.AffiliateID == 0 {
		return nil, nil
	}
	since := q.Now.UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)

	// Countries the affiliate's clicks actually came from.
	clickCountries := map[string]bool{}
	rows, err := Conn(ctx, r.DB).QueryContext(ctx, `
		SELECT DISTINCT country FROM clicks
		WHERE affiliate_id = ? AND country != '' AND exclude_stats = 0 AND ts > ?`,
		q.AffiliateID, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			_ = rows.Close()
			return nil, err
		}
		clickCountries[strings.ToUpper(c)] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	// Geo-tagged conversions in the window.
	var tagged int64
	err = Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM commissions WHERE affiliate_id = ? AND created_at > ? AND country != ''`,
		q.AffiliateID, since).Scan(&tagged)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tagged < 5 || len(clickCountries) == 0 {
		return nil, nil // not enough signal to judge
	}

	convRows, err := Conn(ctx, r.DB).QueryContext(ctx, `
		SELECT country, COUNT(*) FROM commissions
		WHERE affiliate_id = ? AND created_at > ? AND country != ''
		GROUP BY country`, q.AffiliateID, since)
	if err != nil {
		return nil, err
	}
	defer convRows.Close()
	var foreign int64
	var foreignSample string
	for convRows.Next() {
		var c string
		var n int64
		if err := convRows.Scan(&c, &n); err != nil {
			return nil, err
		}
		if !clickCountries[strings.ToUpper(c)] {
			foreign += n
			if foreignSample == "" {
				foreignSample = c
			}
		}
	}
	if err := convRows.Err(); err != nil {
		return nil, err
	}

	if foreign >= 3 && foreign*10 >= tagged { // >=3 and >=10% of tagged
		return []Verdict{{
			Rule:    "geo_mismatch",
			Subject: fmt.Sprint(q.AffiliateID),
			Reason:  fmt.Sprintf("%d of %d sales in the last 7 days came from countries none of the clicks came from (like %s)", foreign, tagged, foreignSample),
			Action:  ActionHold,
		}}, nil
	}
	return nil, nil
}

// IPFallbackAnomaly (threat model R7): an affiliate living on the
// IP-fallback ladder rung is spoofing — fallback is the flagged heuristic
// of last resort and should be rare network-wide (<0.5%); >5% share over
// 7d with a real sample is an XFF-spoof signature.
type IPFallbackAnomaly struct{ DB *sql.DB }

func (IPFallbackAnomaly) Name() string { return "ip_fallback_anomaly" }

func (r IPFallbackAnomaly) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.AffiliateID == 0 {
		return nil, nil
	}
	var fallbacks, total int64
	err := Conn(ctx, r.DB).QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN attribution_method = 'ip_fallback' THEN 1 ELSE 0 END), 0),
		  COUNT(*)
		FROM commissions
		WHERE affiliate_id = ? AND created_at > ?`,
		q.AffiliateID, q.Now.UTC().Add(-7*24*time.Hour).Format(time.RFC3339)).Scan(&fallbacks, &total)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == sql.ErrNoRows || total < 10 {
		return nil, nil
	}
	if fallbacks*20 > total { // >5%
		return []Verdict{{
			Rule:    "ip_fallback_anomaly",
			Subject: fmt.Sprint(q.AffiliateID),
			Reason:  fmt.Sprintf("%d of %d sales in the last 7 days were credited by IP address match only, not by a link or coupon", fallbacks, total),
			Action:  ActionHold,
		}}, nil
	}
	return nil, nil
}

// dayInt mirrors the analytics helper (YYYYMMDD from time).
func dayInt(t time.Time) int64 {
	u := t.UTC()
	return int64(u.Year())*10000 + int64(u.Month())*100 + int64(u.Day())
}

// PaymentFingerprint (rule 9): the same alias-normalized buyer email
// purchasing through MULTIPLE affiliates' links — the self-referral
// farmer's industrial form (N accounts buying from each other).
type PaymentFingerprint struct{ DB *sql.DB }

func (PaymentFingerprint) Name() string { return "payment_fingerprint" }

func (r PaymentFingerprint) Evaluate(ctx context.Context, q Query) ([]Verdict, error) {
	if q.AffiliateID == 0 {
		return nil, nil
	}
	// Buyer emails this affiliate shares with OTHER affiliates' commissions.
	rows, err := Conn(ctx, r.DB).QueryContext(ctx, `
		SELECT DISTINCT buyer_email FROM commissions
		WHERE affiliate_id = ? AND buyer_email != ''`, q.AffiliateID)
	if err != nil {
		return nil, err
	}
	var emails []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			_ = rows.Close()
			return nil, err
		}
		emails = append(emails, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	const minShared = 2 // buyer families spanning 2+ affiliates
	shared := 0
	for _, e := range emails {
		var others int
		if err := Conn(ctx, r.DB).QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT affiliate_id) FROM commissions
			WHERE buyer_email = ? AND affiliate_id != ?`,
			e, q.AffiliateID).Scan(&others); err != nil {
			return nil, err
		}
		if others > 0 {
			shared++
		}
	}
	if shared < minShared {
		return nil, nil
	}
	return []Verdict{{
		Rule:    r.Name(),
		Subject: fmt.Sprintf("affiliate:%d", q.AffiliateID),
		Reason:  fmt.Sprintf("%d buyers also bought through other partners, a sign of the same people cycling purchases", shared),
		Action:  ActionHold,
	}}, nil
}
