package partnermail

// Regular program emails, each switchable in Settings:
//
//   - the owner's Monday report on last week (clicks, sales, commission,
//     top partners), sent directly like the "something needs you" email;
//   - tips for a partner a week in whose link has had no clicks yet;
//   - each partner's monthly recap (last month, and what's ready now).
//
// The partner emails are queued as outbox events and written at send
// time, and honor the partner's "stop program emails" choice. email_log
// makes each one go once per period.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbutil"
	"github.com/thebigasif/affiliate-engine/internal/mail"
	"github.com/thebigasif/affiliate-engine/internal/money"
)

// Outbox topics for the partner emails.
const (
	TopicTips  = "mail.tips"
	TopicRecap = "mail.recap"
)

// When the regular emails go (UTC): the Monday report from 08:00 Monday,
// the recap from 08:00 on the 1st. A late start still sends within the
// catch-up window; after it, that period is skipped.
const (
	sendHour       = 8
	weeklyCatchUp  = 3 * 24 * time.Hour
	recapCatchUp   = 7 * 24 * time.Hour
	tipsAfter      = 7 * 24 * time.Hour
	tipsNotAfter   = 30 * 24 * time.Hour // joined long ago: not "a week in" any more
	periodOnce     = "once"
	kindWeekly     = "weekly"
	kindTips       = "tips"
	kindRecap      = "recap"
	ownerSubjectID = 0
)

type tipsEvent struct {
	AffiliateID int64 `json:"affiliate_id"`
}

type recapEvent struct {
	AffiliateID int64  `json:"affiliate_id"`
	Month       string `json:"month"` // 2026-09
}

// ProgramEmails sends or queues whichever regular emails are due at now.
// It is safe to run often (the engine checks every half hour).
func ProgramEmails(ctx context.Context, db *sql.DB, m mail.Mailer, publicURL, defaultEmail string, now time.Time) error {
	if m == nil {
		return nil
	}
	n, err := LoadOwnerNotify(ctx, db, defaultEmail)
	if err != nil {
		return err
	}
	now = now.UTC()
	var errs []error
	if n.WeeklyReport && n.Email != "" {
		if _, err := WeeklyReport(ctx, db, m, publicURL, n.Email, now); err != nil {
			errs = append(errs, fmt.Errorf("weekly report: %w", err))
		}
	}
	if n.PartnerTips {
		if _, err := QueueTips(ctx, db, now); err != nil {
			errs = append(errs, fmt.Errorf("partner tips: %w", err))
		}
	}
	if n.PartnerRecap {
		if _, err := QueueRecaps(ctx, db, now); err != nil {
			errs = append(errs, fmt.Errorf("partner recaps: %w", err))
		}
	}
	return errors.Join(errs...)
}

// claim records that an email for (kind, subject, period) goes out; false
// when one already did.
func claim(ctx context.Context, x Execer, kind string, subject int64, period string, now time.Time) (bool, error) {
	res, err := x.ExecContext(ctx, `
		INSERT INTO email_log (kind, subject_id, period, sent_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, subject_id, period) DO NOTHING`, kind, subject, period, now.UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// weekStart is 00:00 UTC on the Monday of t's week.
func weekStart(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func dayNum(t time.Time) int { return t.Year()*10000 + int(t.Month())*100 + t.Day() }

type weekTotals struct {
	clicks, sales int64
	earned        money.Totals // per currency, never summed across
}

func totals(ctx context.Context, db *sql.DB, from, to time.Time) (weekTotals, error) {
	var w weekTotals
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0)
		FROM daily_agg WHERE day >= ? AND day <= ?`, dayNum(from), dayNum(to)).Scan(&w.clicks, &w.sales); err != nil {
		return w, err
	}
	var err error
	w.earned, err = earned(ctx, db, 0, from, to)
	return w, err
}

// earned is the commission credited on the UTC days from..to (inclusive)
// per currency, for one affiliate (affID > 0) or the whole program.
// daily_agg's commission_cents adds every currency's minor units into one
// number, so money comes from the commission rows instead.
func earned(ctx context.Context, db *sql.DB, affID int64, from, to time.Time) (money.Totals, error) {
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	rows, err := db.QueryContext(ctx, `
		SELECT currency, COALESCE(SUM(amount),0) FROM commissions
		WHERE amount > 0 AND created_at >= ? AND created_at < ? AND (? = 0 OR affiliate_id = ?)
		GROUP BY currency`, start.Format(time.RFC3339), end.Format(time.RFC3339), affID, affID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := money.Totals{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out.Add(c, v)
	}
	return out, rows.Err()
}

// moneyChange renders per-currency totals with each currency's change on
// the previous period: "$120.00 (up 20% on the week before) and 5,000 JPY".
func moneyChange(cur, prev money.Totals) string {
	if len(cur) == 0 {
		return cur.String()
	}
	parts := make([]string, 0, len(cur))
	for _, t := range cur.List() {
		parts = append(parts, t.Display+change(t.AmountMinor, prev[t.Currency]))
	}
	return strings.Join(parts, " and ")
}

// WeeklyReport sends the owner's report on last week (Monday to Sunday),
// once, on Monday from 08:00 UTC. It returns whether one was sent.
func WeeklyReport(ctx context.Context, db *sql.DB, m mail.Mailer, publicURL, to string, now time.Time) (bool, error) {
	now = now.UTC()
	thisWeek := weekStart(now)
	if now.Before(thisWeek.Add(sendHour*time.Hour)) || now.After(thisWeek.Add(weeklyCatchUp)) {
		return false, nil
	}
	var partners int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM affiliates`).Scan(&partners); err != nil || partners == 0 {
		return false, err // no program yet: nothing to report
	}
	from, until := thisWeek.AddDate(0, 0, -7), thisWeek.AddDate(0, 0, -1)
	y, wk := from.ISOWeek()
	period := fmt.Sprintf("%d-W%02d", y, wk)
	ok, err := claim(ctx, db, kindWeekly, ownerSubjectID, period, now)
	if err != nil || !ok {
		return false, err
	}
	sent := false
	defer func() {
		if !sent { // let the next check try again
			_, _ = db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM email_log WHERE kind = ? AND subject_id = ? AND period = ?`, kindWeekly, ownerSubjectID, period)
		}
	}()

	cur, err := totals(ctx, db, from, until)
	if err != nil {
		return false, err
	}
	prev, err := totals(ctx, db, from.AddDate(0, 0, -7), until.AddDate(0, 0, -7))
	if err != nil {
		return false, err
	}
	// Top partners by sales: commission in different currencies does not
	// rank (no FX), so each one's earnings are listed per currency.
	rows, err := db.QueryContext(ctx, `
		SELECT d.affiliate_id, a.email, a.name, SUM(d.conversions)
		FROM daily_agg d JOIN affiliates a ON a.id = d.affiliate_id
		WHERE d.day >= ? AND d.day <= ?
		GROUP BY d.affiliate_id, a.email, a.name
		HAVING SUM(d.conversions) > 0
		ORDER BY SUM(d.conversions) DESC, d.affiliate_id LIMIT 3`, dayNum(from), dayNum(until))
	if err != nil {
		return false, err
	}
	type topRow struct {
		id, sales   int64
		email, name string
	}
	var topRows []topRow
	for rows.Next() {
		var x topRow
		if err := rows.Scan(&x.id, &x.email, &x.name, &x.sales); err != nil {
			_ = rows.Close()
			return false, err
		}
		topRows = append(topRows, x)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	var top []string
	for _, x := range topRows {
		who := strings.TrimSpace(x.name)
		if who == "" {
			who = x.email
		}
		got, err := earned(ctx, db, x.id, from, until)
		if err != nil {
			return false, err
		}
		top = append(top, fmt.Sprintf("%d. %s: %s from %s", len(top)+1, who, got, count(x.sales, "sale", "sales")))
	}

	prog, _ := (&Sink{DB: db, PublicURL: publicURL}).program(ctx)
	base := strings.TrimRight(publicURL, "/")
	span := from.Format("Jan 2") + " to " + until.Format("Jan 2")
	var b strings.Builder
	b.WriteString("Hi,\n\nHere's how " + prog + " did last week (" + span + "):\n\n")
	b.WriteString("- Clicks: " + commas(cur.clicks) + change(cur.clicks, prev.clicks) + "\n")
	b.WriteString("- Sales: " + commas(cur.sales) + change(cur.sales, prev.sales) + "\n")
	b.WriteString("- Commission earned: " + moneyChange(cur.earned, prev.earned) + "\n")
	if len(top) > 0 {
		b.WriteString("\nTop partners:\n" + strings.Join(top, "\n") + "\n")
	}
	if cur.clicks == 0 && cur.sales == 0 {
		b.WriteString("\nA quiet week. A short note to your partners, like a new page to link to or a better deal this month, often gets things moving: " + base + "/#/partners\n")
	}
	b.WriteString("\nThe full picture: " + base + "/#/overview\n")
	b.WriteString("\nYou get this every Monday. Turn it off in Settings: " + base + "/#/settings\n")
	subject := prog + ": last week, " + commas(cur.clicks) + " " + plural(cur.clicks, "click", "clicks") + " and " + count(cur.sales, "sale", "sales")
	if err := m.Send(ctx, mail.Mail{To: to, Subject: subject, Text: b.String(), FromName: prog, Tag: "weekly-report"}); err != nil {
		return false, err
	}
	sent = true
	return true, nil
}

// change is " (up 12% on the week before)", or "" when there's nothing
// to compare with.
func change(cur, prev int64) string {
	if prev <= 0 {
		return ""
	}
	pct := (cur - prev) * 100 / prev
	switch {
	case pct > 0:
		return fmt.Sprintf(" (up %d%% on the week before)", pct)
	case pct < 0:
		return fmt.Sprintf(" (down %d%% on the week before)", -pct)
	}
	return " (same as the week before)"
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func count(n int64, one, many string) string { return commas(n) + " " + plural(n, one, many) }

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// QueueTips queues the tips email for each active partner who joined 7 to
// 30 days ago and whose link hasn't had a click. Once per partner.
func QueueTips(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.created_at FROM affiliates a
		WHERE a.status = 'active' AND a.news_opt_out = 0
		  AND NOT EXISTS (SELECT 1 FROM clicks c WHERE c.affiliate_id = a.id)
		  AND NOT EXISTS (SELECT 1 FROM email_log l WHERE l.kind = 'tips' AND l.subject_id = a.id)
		ORDER BY a.id`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var created any
		if err := rows.Scan(&id, &created); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if t, ok := dbutil.AsTime(created); ok && now.Sub(t) >= tipsAfter && now.Sub(t) <= tipsNotAfter {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return queueEach(ctx, db, ids, kindTips, periodOnce, now, func(tx *sql.Tx, id int64) error {
		return enqueue(ctx, tx, TopicTips, tipsEvent{AffiliateID: id})
	})
}

// QueueRecaps queues last month's recap, from 08:00 UTC on the 1st, for
// each active partner who had clicks or earnings last month or has money
// waiting now.
func QueueRecaps(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	now = now.UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if now.Before(monthStart.Add(sendHour*time.Hour)) || now.After(monthStart.Add(recapCatchUp)) {
		return 0, nil
	}
	from := monthStart.AddDate(0, -1, 0)
	until := monthStart.AddDate(0, 0, -1)
	month := from.Format("2006-01")
	rows, err := db.QueryContext(ctx, `
		SELECT a.id FROM affiliates a
		WHERE a.status = 'active' AND a.news_opt_out = 0
		  AND NOT EXISTS (SELECT 1 FROM email_log l WHERE l.kind = 'recap' AND l.subject_id = a.id AND l.period = ?)
		  AND (EXISTS (SELECT 1 FROM daily_agg d WHERE d.affiliate_id = a.id AND d.day >= ? AND d.day <= ?
		               AND (d.clicks > 0 OR d.commission_cents <> 0))
		       OR EXISTS (SELECT 1 FROM commissions c WHERE c.affiliate_id = a.id AND c.status IN ('pending', 'approved')))
		ORDER BY a.id`, month, dayNum(from), dayNum(until))
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return queueEach(ctx, db, ids, kindRecap, month, now, func(tx *sql.Tx, id int64) error {
		return enqueue(ctx, tx, TopicRecap, recapEvent{AffiliateID: id, Month: month})
	})
}

// queueEach claims and queues one email per partner, each in its own
// transaction, so the log and the queue always agree.
func queueEach(ctx context.Context, db *sql.DB, ids []int64, kind, period string, now time.Time, queue func(*sql.Tx, int64) error) (int, error) {
	queued := 0
	for _, id := range ids {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return queued, err
		}
		ok, err := claim(ctx, tx, kind, id, period, now)
		if err == nil && ok {
			err = queue(tx, id)
		}
		if err != nil || !ok {
			_ = tx.Rollback()
			if err != nil {
				return queued, err
			}
			continue
		}
		if err := tx.Commit(); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

// partnerForProgramMail loads a partner who may get a program email:
// active, and hasn't stopped program emails. ok=false means drop it.
func (s *Sink) partnerForProgramMail(ctx context.Context, id int64) (affiliate, bool, error) {
	var a affiliate
	var name sql.NullString
	var optOut int
	err := s.DB.QueryRowContext(ctx, `SELECT email, name, status, news_opt_out FROM affiliates WHERE id = ?`, id).
		Scan(&a.email, &name, &a.status, &optOut)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	a.name = strings.TrimSpace(name.String)
	return a, a.status == "active" && optOut == 0 && len(s.NewsKey) > 0, nil
}

func (s *Sink) referralLink(ctx context.Context, base string, id int64) string {
	var code sql.NullString
	_ = s.DB.QueryRowContext(ctx, `
		SELECT code FROM affiliate_codes WHERE affiliate_id = ?
		ORDER BY is_primary DESC, id LIMIT 1`, id).Scan(&code)
	if code.String == "" {
		return ""
	}
	return base + "/r/" + code.String
}

func (s *Sink) programFooter(prog, unsub string) string {
	return "--\nYou get this because you're a partner in " + prog + ". Stop program emails (sign-in links and payout notes still come): " + unsub + "\n"
}

func (s *Sink) tips(ctx context.Context, e tipsEvent) error {
	a, ok, err := s.partnerForProgramMail(ctx, e.AffiliateID)
	if err != nil || !ok {
		return err
	}
	var clicked int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM clicks WHERE affiliate_id = ?`, e.AffiliateID).Scan(&clicked); err != nil {
		return err
	}
	if clicked > 0 {
		return nil // got going since: no tips needed
	}
	prog, base := s.program(ctx)
	if base == "" {
		return errors.New("partnermail: no public URL to link to (set AE_PUBLIC_URL)")
	}
	unsub := base + "/api/v1/portal/unsubscribe?t=" + UnsubscribeToken(s.NewsKey, e.AffiliateID)
	var b strings.Builder
	b.WriteString(greeting(a.name) + "\n\n")
	b.WriteString("You joined " + prog + " about a week ago. Your link hasn't had a click yet, so here are three quick ways to start.\n\n")
	if link := s.referralLink(ctx, base, e.AffiliateID); link != "" {
		b.WriteString("Your link: " + link + "\n\n")
	}
	b.WriteString("1. Put it where people already ask you what you use: your bio, your newsletter footer or a resources page.\n")
	b.WriteString("2. Write one honest post or email about how you use it. A real story works better than an ad.\n")
	b.WriteString("3. Link to the page that sells: pricing or a feature page often works better than the homepage. You can make a link to any page in your partner portal.\n\n")
	if deal := s.deal(ctx, e.AffiliateID); len(deal) > 0 {
		b.WriteString("Your deal:\n")
		for _, d := range deal {
			b.WriteString("- " + d + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Your partner portal: " + base + "/\n\n")
	b.WriteString(s.programFooter(prog, unsub))
	return s.Mailer.Send(ctx, mail.Mail{To: a.email, Subject: "3 easy ways to share your " + prog + " link", Text: b.String(), FromName: prog, Tag: "partner-tips", ListUnsubscribe: unsub})
}

func (s *Sink) recap(ctx context.Context, e recapEvent) error {
	from, err := time.Parse("2006-01", e.Month)
	if err != nil {
		return nil // malformed: nothing a retry fixes
	}
	a, ok, err := s.partnerForProgramMail(ctx, e.AffiliateID)
	if err != nil || !ok {
		return err
	}
	until := from.AddDate(0, 1, -1)
	var clicks, sales int64
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(clicks),0), COALESCE(SUM(conversions),0)
		FROM daily_agg WHERE affiliate_id = ? AND day >= ? AND day <= ?`,
		e.AffiliateID, dayNum(from), dayNum(until)).Scan(&clicks, &sales); err != nil {
		return err
	}
	got, err := earned(ctx, s.DB, e.AffiliateID, from, until)
	if err != nil {
		return err
	}
	ready, held, err := s.balances(ctx, e.AffiliateID)
	if err != nil {
		return err
	}
	if clicks == 0 && len(got) == 0 && len(ready) == 0 && len(held) == 0 {
		return nil
	}
	prog, base := s.program(ctx)
	if base == "" {
		return errors.New("partnermail: no public URL to link to (set AE_PUBLIC_URL)")
	}
	unsub := base + "/api/v1/portal/unsubscribe?t=" + UnsubscribeToken(s.NewsKey, e.AffiliateID)
	mon := from.Format("January")
	var b strings.Builder
	b.WriteString(greeting(a.name) + "\n\n")
	b.WriteString("Here's your " + mon + " with " + prog + ":\n\n")
	b.WriteString("- Clicks: " + commas(clicks) + "\n")
	b.WriteString("- Sales: " + commas(sales) + "\n")
	b.WriteString("- Commission earned: " + got.String() + "\n")
	if len(ready) > 0 || len(held) > 0 {
		b.WriteString("\nRight now:\n")
		if len(ready) > 0 {
			b.WriteString("- Ready for your next payout: " + strings.Join(ready, " and ") + "\n")
		}
		if len(held) > 0 {
			b.WriteString("- In hold: " + strings.Join(held, " and ") + ". It becomes ready once the refund window passes.\n")
		}
	}
	var hasDetails int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM affiliate_payout_details WHERE affiliate_id = ?`, e.AffiliateID).Scan(&hasDetails)
	if hasDetails == 0 && (len(ready) > 0 || len(held) > 0) {
		b.WriteString("\nTell " + prog + " where to send your money under \"How you get paid\" in your partner portal.\n")
	}
	if link := s.referralLink(ctx, base, e.AffiliateID); link != "" {
		b.WriteString("\nYour link: " + link + "\n")
	}
	b.WriteString("Your partner portal: " + base + "/\n\n")
	b.WriteString(s.programFooter(prog, unsub))
	subject := "Your " + mon + " with " + prog + ": " + got.String() + " earned"
	return s.Mailer.Send(ctx, mail.Mail{To: a.email, Subject: subject, Text: b.String(), FromName: prog, Tag: "partner-recap", ListUnsubscribe: unsub})
}

// balances is what's ready to pay and what's in hold, one amount per
// currency.
func (s *Sink) balances(ctx context.Context, id int64) (ready, held []string, err error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT status, currency, SUM(amount) FROM commissions
		WHERE affiliate_id = ? AND status IN ('approved', 'pending')
		GROUP BY status, currency`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status, cur string
		var amt int64
		if err := rows.Scan(&status, &cur, &amt); err != nil {
			return nil, nil, err
		}
		if amt <= 0 {
			continue
		}
		if status == "approved" {
			ready = append(ready, Money(amt, cur))
		} else {
			held = append(held, Money(amt, cur))
		}
	}
	sort.Strings(ready)
	sort.Strings(held)
	return ready, held, rows.Err()
}

func decodeTips(p []byte) (tipsEvent, bool) {
	var e tipsEvent
	return e, json.Unmarshal(p, &e) == nil && e.AffiliateID > 0
}

func decodeRecap(p []byte) (recapEvent, bool) {
	var e recapEvent
	return e, json.Unmarshal(p, &e) == nil && e.AffiliateID > 0 && e.Month != ""
}
