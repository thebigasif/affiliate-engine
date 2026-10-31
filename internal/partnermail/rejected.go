package partnermail

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/mail"
)

// TopicRejected is the note to someone whose application was declined.
const TopicRejected = "mail.application_rejected"

type rejectedEvent struct {
	ApplicationID int64  `json:"application_id"`
	Reason        string `json:"reason,omitempty"`
}

// EnqueueRejected queues the "your application" note to an applicant the
// owner declined, with their reason if they gave one.
func EnqueueRejected(ctx context.Context, x Execer, applicationID int64, reason string) error {
	return enqueue(ctx, x, TopicRejected, rejectedEvent{ApplicationID: applicationID, Reason: strings.TrimSpace(reason)})
}

func (s *Sink) rejected(ctx context.Context, e rejectedEvent) error {
	var email, name, status string
	err := s.DB.QueryRowContext(ctx, `SELECT email, name, status FROM affiliate_applications WHERE id = ?`, e.ApplicationID).
		Scan(&email, &name, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "rejected" {
		return nil // reviewed again since
	}
	prog, _ := s.program(ctx)
	var b strings.Builder
	b.WriteString(greeting(name) + "\n\n")
	b.WriteString("Thanks for applying to " + prog + ". We can't accept your application right now.\n\n")
	if e.Reason != "" {
		b.WriteString(e.Reason + "\n\n")
	}
	b.WriteString("Thanks again for your interest in working with us.\n\n" + prog + "\n")
	return s.Mailer.Send(ctx, mail.Mail{To: email, Subject: "Your application to " + prog, Text: b.String(), FromName: prog, Tag: "application-rejected"})
}
