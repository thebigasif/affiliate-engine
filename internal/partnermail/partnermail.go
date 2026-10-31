// Package partnermail emails affiliates: a login link when they are
// approved or ask for one, and a note when a payout is approved.
//
// Requests are queued as outbox events (Enqueue*) and sent by Sink, so a
// provider outage retries instead of losing the email. A login-link event
// carries only the affiliate id: the grant is minted at send time, so no
// usable secret sits in the queue.
package partnermail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/mail"
	"github.com/thebigasif/affiliate-engine/internal/money"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
	"github.com/thebigasif/affiliate-engine/internal/settings"
)

// Outbox topics.
const (
	TopicLoginLink  = "mail.login_link"
	TopicPayoutPaid = "mail.payout_paid"
)

// Why a login link is sent.
const (
	ReasonApproved      = "approved"
	ReasonRequested     = "requested"
	ReasonPayoutDetails = "payout_details" // a payout waits; they haven't said where to be paid
)

// Grant lifetimes: an approval email may sit unread for a day; a link the
// affiliate just asked for is used within minutes.
const (
	approvedGrantTTL  = 24 * time.Hour
	requestedGrantTTL = time.Hour
)

// Execer is a *sql.DB or *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type loginLinkEvent struct {
	AffiliateID int64  `json:"affiliate_id"`
	Reason      string `json:"reason"`
}

type payoutEvent struct {
	PayoutID int64 `json:"payout_id"`
}

func enqueue(ctx context.Context, x Execer, topic string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = x.ExecContext(ctx, `INSERT INTO outbox_events (topic, payload) VALUES (?, ?)`, topic, string(b))
	return err
}

// EnqueueLoginLink queues a login-link email to an affiliate.
func EnqueueLoginLink(ctx context.Context, x Execer, affiliateID int64, reason string) error {
	return enqueue(ctx, x, TopicLoginLink, loginLinkEvent{AffiliateID: affiliateID, Reason: reason})
}

// EnqueuePayoutPaid queues the "you've been paid" email for a payout.
func EnqueuePayoutPaid(ctx context.Context, x Execer, payoutID int64) error {
	return enqueue(ctx, x, TopicPayoutPaid, payoutEvent{PayoutID: payoutID})
}

// Sink sends the queued partner emails.
type Sink struct {
	DB        *sql.DB
	Mailer    mail.Mailer
	PublicURL string // the engine's own address (AE_PUBLIC_URL)
	NewsKey   []byte // signs unsubscribe links (DeriveNewsKey); nil = news not sent
	Now       func() time.Time
}

func (s *Sink) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Deliver implements outbox.Sink. An email that no longer applies (the
// affiliate was frozen, the payout isn't paid) is dropped, not retried.
func (s *Sink) Deliver(ctx context.Context, ev outbox.Event) error {
	switch ev.Topic {
	case TopicLoginLink:
		var e loginLinkEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil || e.AffiliateID <= 0 {
			return nil // malformed: nothing a retry fixes
		}
		return s.loginLink(ctx, e)
	case TopicNews:
		var e newsEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil || e.AffiliateID <= 0 {
			return nil
		}
		return s.news(ctx, e)
	case TopicRejected:
		var e rejectedEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil || e.ApplicationID <= 0 {
			return nil
		}
		return s.rejected(ctx, e)
	case TopicTips:
		if e, ok := decodeTips(ev.Payload); ok {
			return s.tips(ctx, e)
		}
		return nil
	case TopicRecap:
		if e, ok := decodeRecap(ev.Payload); ok {
			return s.recap(ctx, e)
		}
		return nil
	case TopicPayoutPaid:
		var e payoutEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil || e.PayoutID <= 0 {
			return nil
		}
		return s.payoutPaid(ctx, e)
	}
	return nil
}

type affiliate struct {
	email, name, status string
}

func (s *Sink) affiliate(ctx context.Context, id int64) (affiliate, error) {
	var a affiliate
	var name sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT email, name, status FROM affiliates WHERE id = ?`, id).
		Scan(&a.email, &name, &a.status)
	a.name = strings.TrimSpace(name.String)
	return a, err
}

// program returns the program's name and the portal's base URL (the
// claimed portal domain, else the engine's public URL).
func (s *Sink) program(ctx context.Context) (name, base string) {
	var brand, domain sql.NullString
	_ = s.DB.QueryRowContext(ctx, `SELECT brand_name, portal_domain FROM program_branding WHERE campaign_id = 1`).Scan(&brand, &domain)
	name = strings.TrimSpace(brand.String)
	if name == "" {
		var camp sql.NullString
		_ = s.DB.QueryRowContext(ctx, `SELECT name FROM campaigns WHERE id = 1`).Scan(&camp)
		name = strings.TrimSpace(camp.String)
	}
	if name == "" {
		name = "Partner program"
	}
	if d := strings.TrimSpace(domain.String); d != "" {
		return name, "https://" + d
	}
	return name, strings.TrimRight(s.PublicURL, "/")
}

// greeting uses the whole name: partner names are often a business ("Deep
// Work Weekly"), where a first word alone reads wrong.
func greeting(name string) string {
	if n := strings.Join(strings.Fields(name), " "); n != "" {
		return "Hi " + n + ","
	}
	return "Hi,"
}

func (s *Sink) loginLink(ctx context.Context, e loginLinkEvent) error {
	a, err := s.affiliate(ctx, e.AffiliateID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.status != "active" {
		return nil // frozen or removed since: no way in
	}
	prog, base := s.program(ctx)
	if base == "" {
		return fmt.Errorf("partnermail: no public URL to link to (set AE_PUBLIC_URL)")
	}

	var waiting []string // payout_details: what's waiting, per currency
	if e.Reason == ReasonPayoutDetails {
		var has int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM affiliate_payout_details WHERE affiliate_id = ?`, e.AffiliateID).Scan(&has); err != nil {
			return err
		}
		if waiting, err = s.waitingPayouts(ctx, e.AffiliateID); err != nil {
			return err
		}
		if has > 0 || len(waiting) == 0 {
			return nil // added since, or nothing waits any more
		}
	}

	ttl := requestedGrantTTL
	if e.Reason == ReasonApproved || e.Reason == ReasonPayoutDetails {
		ttl = approvedGrantTTL
	}
	grant, err := newGrant()
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO portal_login_grants (token_hash, affiliate_id, expires_at) VALUES (?, ?, ?)`,
		hashGrant(grant), e.AffiliateID, s.now().Add(ttl).Format(time.RFC3339)); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString(greeting(a.name) + "\n\n")
	var subject string
	switch e.Reason {
	case ReasonPayoutDetails:
		subject = "Where should " + prog + " send your money?"
		b.WriteString(prog + " has " + strings.Join(waiting, " and ") + " ready for you, but doesn't know where to send it yet.\n\n")
		b.WriteString("Add your PayPal or Wise email, or your bank details, under \"How you get paid\" in your partner portal. This link signs you in and takes you there:\n" + base + "/#grant=" + grant + "&to=payto\n\n")
		b.WriteString("It works once, for 24 hours. After that, get a new one at " + base + "/\n")
	case ReasonApproved:
		// What they came for first: the link to share and what it earns,
		// then how to sign in.
		subject = "You're in: " + prog
		b.WriteString("Your application to " + prog + " was approved. Welcome aboard.\n\n")
		var code sql.NullString
		_ = s.DB.QueryRowContext(ctx, `
			SELECT code FROM affiliate_codes WHERE affiliate_id = ?
			ORDER BY is_primary DESC, id LIMIT 1`, e.AffiliateID).Scan(&code)
		if code.String != "" {
			b.WriteString("Your referral link:\n" + base + "/r/" + code.String + "\n\n")
			b.WriteString("Share it anywhere. Everyone who buys through it is credited to you.\n\n")
		}
		if deal := s.deal(ctx, e.AffiliateID); len(deal) > 0 {
			b.WriteString("Your deal:\n")
			for _, d := range deal {
				b.WriteString("- " + d + "\n")
			}
			b.WriteString("\n")
		}
		b.WriteString("See your clicks, sales and earnings in your partner portal. This link signs you in:\n" + base + "/#grant=" + grant + "\n\n")
		b.WriteString("It works once, for 24 hours. After that, get a new one at " + base + "/\n")
	default:
		subject = "Your login link for " + prog
		b.WriteString("Here's the sign-in link you asked for:\n" + base + "/#grant=" + grant + "\n\n")
		b.WriteString("It works once and expires in 1 hour. If you didn't ask for it, ignore this email: nobody can sign in without it.\n")
	}
	tag := "login-link"
	if e.Reason == ReasonPayoutDetails {
		tag = "payout-details"
	}
	return s.Mailer.Send(ctx, mail.Mail{To: a.email, Subject: subject, Text: b.String(), FromName: prog, Tag: tag})
}

// waitingPayouts is what's ready for a partner and not yet paid, one
// amount per currency ("$120.00").
func (s *Sink) waitingPayouts(ctx context.Context, affID int64) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT currency, SUM(amount) FROM payouts
		WHERE affiliate_id = ? AND status = 'requested'
		GROUP BY currency ORDER BY currency`, affID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cur string
		var amt int64
		if err := rows.Scan(&cur, &amt); err != nil {
			return nil, err
		}
		if amt > 0 {
			out = append(out, Money(amt, cur))
		}
	}
	return out, rows.Err()
}

func (s *Sink) payoutPaid(ctx context.Context, e payoutEvent) error {
	var affID, amount int64
	var currency, method, status string
	err := s.DB.QueryRowContext(ctx, `
		SELECT affiliate_id, amount, currency, method, status FROM payouts WHERE id = ?`, e.PayoutID).
		Scan(&affID, &amount, &currency, &method, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "paid" {
		return nil
	}
	a, err := s.affiliate(ctx, affID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	prog, base := s.program(ctx)
	amt := Money(amount, currency)
	var b strings.Builder
	b.WriteString(greeting(a.name) + "\n\n")
	b.WriteString(prog + " paid you " + amt)
	if m := methodName(method); m != "" {
		b.WriteString(" by " + m)
	}
	b.WriteString(" (payout #" + strconv.FormatInt(e.PayoutID, 10) + ").\n")
	if base != "" {
		b.WriteString("\nEvery commission it covers is listed in your partner portal: " + base + "/\n")
	}
	return s.Mailer.Send(ctx, mail.Mail{To: a.email, Subject: prog + " paid you " + amt, Text: b.String(), FromName: prog, Tag: "payout-paid"})
}

func methodName(m string) string {
	switch m {
	case "paypal":
		return "PayPal"
	case "wise":
		return "Wise"
	case "stripe", "stripe_connect":
		return "Stripe"
	}
	return ""
}

// Money formats minor units by the currency's ISO 4217 exponent:
// $1,234.50, €12.00, 12.00 CHF, 5,000 JPY or 1.000 KWD.
func Money(cents int64, currency string) string {
	return money.Display(cents, currency)
}

// newGrant and hashGrant match the portal's login grants (httpapi): an
// aps_ token, stored as its SHA-256.
func newGrant() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "aps_" + hex.EncodeToString(b), nil
}

func hashGrant(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// deal is the partner's offer in plain words, as the portal and /apply
// show it: their own rate if they have one, else the program default.
func (s *Sink) deal(ctx context.Context, affID int64) []string {
	var rateType string
	var rateValue int64
	var rateCur sql.NullString
	if err := s.DB.QueryRowContext(ctx, `
		SELECT rate_type, rate_value, currency FROM commission_rules
		WHERE campaign_id = 1 AND active = TRUE AND product_key IS NULL
		  AND (affiliate_id = ? OR affiliate_id IS NULL)
		ORDER BY CASE WHEN affiliate_id IS NULL THEN 1 ELSE 0 END, priority DESC LIMIT 1`, affID).
		Scan(&rateType, &rateValue, &rateCur); err != nil {
		return nil
	}
	// A fixed rate is money in its rule's currency (else the program's).
	fixedCur := settings.Currency(ctx, s.DB)
	if rateCur.String != "" {
		fixedCur = rateCur.String
	}
	var months, cookie int
	_ = s.DB.QueryRowContext(ctx, `SELECT commission_months, attribution_window_days FROM campaigns WHERE id = 1`).Scan(&months, &cookie)
	var out []string
	rate := Money(rateValue, fixedCur) + " for every payment"
	if rateType == "percent" { // basis points: 3000 is 30%, 1250 is 12.5%
		pct := strconv.FormatInt(rateValue/100, 10)
		if frac := rateValue % 100; frac != 0 {
			pct += "." + strings.TrimRight(fmt.Sprintf("%02d", frac), "0")
		}
		rate = pct + "% of every payment"
	}
	switch {
	case months == 1:
		out = append(out, rate+" from customers you send, for their first month")
	case months > 1:
		out = append(out, fmt.Sprintf("%s from customers you send, for their first %d months", rate, months))
	default:
		out = append(out, rate+" from customers you send, for as long as they stay")
	}
	if cookie > 0 {
		out = append(out, fmt.Sprintf("A %d-day cookie: you're credited if they buy within %d days of clicking", cookie, cookie))
	}
	if p, err := settings.Load(ctx, s.DB); err == nil && p.MinPayoutCents > 0 {
		out = append(out, "Paid once you've earned "+Money(p.MinPayoutCents, p.Currency))
	}
	return out
}
