package partnermail

// The program owner's "something needs you" email: at most one a day,
// only when something is waiting (applications, held commissions, payouts
// to send, tax forms to review) and only when something new arrived since
// the last one, so an unchanged pile isn't sent twice.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/mail"
)

// OwnerNotify is where the owner's email goes and whether it's on.
type OwnerNotify struct {
	Email   string `json:"email"`
	Enabled bool   `json:"enabled"` // the "something needs you" email

	// The regular emails (programmail.go).
	WeeklyReport bool `json:"weekly_report"` // the owner's Monday report
	PartnerTips  bool `json:"partner_tips"`  // tips for partners with no clicks a week in
	PartnerRecap bool `json:"partner_recap"` // each partner's monthly recap
}

// LoadOwnerNotify reads the setting. With no saved row it's on, to
// defaultEmail (AE_OWNER_EMAIL; the account email on Cloud).
func LoadOwnerNotify(ctx context.Context, db *sql.DB, defaultEmail string) (OwnerNotify, error) {
	n := OwnerNotify{Email: defaultEmail, Enabled: true, WeeklyReport: true, PartnerTips: true, PartnerRecap: true}
	var email string
	var enabled int
	err := db.QueryRowContext(ctx, `SELECT email, enabled FROM owner_notify WHERE id = 1`).Scan(&email, &enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerNotify{}, err
	}
	if err == nil {
		if email != "" {
			n.Email = email
		}
		n.Enabled = enabled == 1
	}
	var weekly, tips, recap int
	err = db.QueryRowContext(ctx, `SELECT weekly_report, partner_tips, partner_recap FROM program_emails WHERE id = 1`).Scan(&weekly, &tips, &recap)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerNotify{}, err
	}
	if err == nil {
		n.WeeklyReport, n.PartnerTips, n.PartnerRecap = weekly == 1, tips == 1, recap == 1
	}
	return n, nil
}

// SaveOwnerNotify stores the setting.
func SaveOwnerNotify(ctx context.Context, db *sql.DB, n OwnerNotify) error {
	enabled := b2i(n.Enabled)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO owner_notify (id, email, enabled) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET email = excluded.email, enabled = excluded.enabled`,
		strings.TrimSpace(n.Email), enabled); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO program_emails (id, weekly_report, partner_tips, partner_recap) VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET weekly_report = excluded.weekly_report,
		  partner_tips = excluded.partner_tips, partner_recap = excluded.partner_recap`,
		b2i(n.WeeklyReport), b2i(n.PartnerTips), b2i(n.PartnerRecap))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ownerMinGap: at most one email per day (a little under, so a daily
// check at a drifting time still sends).
const ownerMinGap = 20 * time.Hour

type waiting struct {
	n      int64
	newest int64
	one    string    // "person applied to become a partner"
	many   string    // "people applied to become partners"
	page   string    // dashboard page
	noun   [2]string // for the subject: one, many
}

// OwnerDigest sends the owner's email if something needs them. It returns
// whether one was sent. publicURL is the dashboard's address.
func OwnerDigest(ctx context.Context, db *sql.DB, m mail.Mailer, publicURL, defaultEmail string, now time.Time) (bool, error) {
	if m == nil {
		return false, nil
	}
	n, err := LoadOwnerNotify(ctx, db, defaultEmail)
	if err != nil || !n.Enabled || n.Email == "" {
		return false, err
	}
	items := []waiting{
		{one: "person applied to become a partner", many: "people applied to become partners", page: "applications", noun: [2]string{"application", "applications"}},
		{one: "commission is held by fraud checks and needs your decision", many: "commissions are held by fraud checks and need your decision", page: "fraud", noun: [2]string{"held commission", "held commissions"}},
		{one: "payout is ready to send", many: "payouts are ready to send", page: "payouts", noun: [2]string{"payout to send", "payouts to send"}},
		{one: "tax form is waiting for your review", many: "tax forms are waiting for your review", page: "taxforms", noun: [2]string{"tax form", "tax forms"}},
	}
	queries := []string{
		`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM affiliate_applications WHERE status = 'pending'`,
		`SELECT COUNT(*), COALESCE(MAX(commission_id), 0) FROM commission_holds WHERE resolved_at IS NULL`,
		`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM payouts WHERE status = 'requested'`,
		`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM tax_documents WHERE status = 'received'`,
	}
	var keyParts []string
	total := 0
	for i, q := range queries {
		if err := db.QueryRowContext(ctx, q).Scan(&items[i].n, &items[i].newest); err != nil {
			return false, err
		}
		keyParts = append(keyParts, fmt.Sprintf("%s:%d", items[i].page, items[i].newest))
		if items[i].n > 0 {
			total++
		}
	}
	if total == 0 {
		return false, nil
	}
	key := strings.Join(keyParts, "|")
	var lastSent sql.NullString
	var lastKey string
	err = db.QueryRowContext(ctx, `SELECT last_sent_at, last_key FROM owner_notify WHERE id = 1`).Scan(&lastSent, &lastKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if key == lastKey {
		return false, nil // nothing new since the last email
	}
	if t, err := time.Parse(time.RFC3339, lastSent.String); err == nil && now.Sub(t) < ownerMinGap {
		return false, nil
	}

	prog, _ := (&Sink{DB: db, PublicURL: publicURL}).program(ctx)
	base := strings.TrimRight(publicURL, "/")
	var b strings.Builder
	b.WriteString("Hi,\n\nThese are waiting for you in " + prog + ":\n\n")
	var subj []string
	for _, it := range items {
		if it.n == 0 {
			continue
		}
		what, noun := it.many, it.noun[1]
		if it.n == 1 {
			what, noun = it.one, it.noun[0]
		}
		b.WriteString(fmt.Sprintf("- %d %s: %s/#/%s\n", it.n, what, base, it.page))
		subj = append(subj, fmt.Sprintf("%d %s", it.n, noun))
	}
	b.WriteString("\nYou get this at most once a day, and only when something new is waiting. Turn it off in Settings: " + base + "/#/settings\n")
	subject := prog + ": " + strings.Join(subj, ", ")
	if err := m.Send(ctx, mail.Mail{To: n.Email, Subject: subject, Text: b.String(), FromName: prog, Tag: "owner-digest"}); err != nil {
		return false, err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO owner_notify (id, email, enabled, last_sent_at, last_key) VALUES (1, '', 1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET last_sent_at = excluded.last_sent_at, last_key = excluded.last_key`,
		now.UTC().Format(time.RFC3339), key)
	return true, err
}
