package partnermail

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/mail"
)

// TopicPayoutDetailsChanged is the notice to a partner that their payout
// details changed. It goes to their login address, so a partner whose
// session or key was stolen learns of the change.
const TopicPayoutDetailsChanged = "mail.payout_details_changed"

// PayoutDetailsChange is what the notice says. Before and After are
// masked values only ("PayPal: j***@example.com", "Bank: **** 1234"),
// never the full identifier. CoolingUntil is zero when the change pays at once (a
// first-ever setup).
type PayoutDetailsChange struct {
	AffiliateID  int64     `json:"affiliate_id"`
	Method       string    `json:"method"`
	Before       string    `json:"before,omitempty"`
	After        string    `json:"after"`
	CoolingUntil time.Time `json:"cooling_until,omitzero"`
}

// EnqueuePayoutDetailsChanged queues the change notice.
func EnqueuePayoutDetailsChanged(ctx context.Context, x Execer, c PayoutDetailsChange) error {
	return enqueue(ctx, x, TopicPayoutDetailsChanged, c)
}

func (s *Sink) payoutDetailsChanged(ctx context.Context, c PayoutDetailsChange) error {
	a, err := s.affiliate(ctx, c.AffiliateID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.status == "removed" {
		return nil // erased since: no address to write to
	}
	prog, base := s.program(ctx)
	var b strings.Builder
	b.WriteString(greeting(a.name) + "\n\n")
	b.WriteString("The details " + prog + " uses to pay you were changed.\n\n")
	before := c.Before
	if before == "" {
		before = "none"
	}
	b.WriteString("Before: " + before + "\n")
	b.WriteString("Now: " + c.After + "\n\n")
	if !c.CoolingUntil.IsZero() {
		b.WriteString("For your safety, payouts to the new details wait until " +
			c.CoolingUntil.UTC().Format("2 January 2006 at 15:04 UTC") + ".\n\n")
	}
	b.WriteString("If you made this change, you don't need to do anything.\n")
	b.WriteString("If you didn't, sign in to your partner portal and change it back, then tell " + prog + " right away")
	if base != "" {
		b.WriteString(": " + base + "/")
	}
	b.WriteString("\n")
	return s.Mailer.Send(ctx, mail.Mail{To: a.email, Subject: "Your payout details for " + prog + " changed", Text: b.String(), FromName: prog, Tag: "payout-details-changed"})
}
