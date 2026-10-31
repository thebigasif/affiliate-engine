package partnermail

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/mail"
)

// TopicNews is one program-news email to one partner.
const TopicNews = "mail.news"

type newsEvent struct {
	AffiliateID int64  `json:"affiliate_id"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
}

// EnqueueNews queues a program-news email to one partner.
func EnqueueNews(ctx context.Context, x Execer, affiliateID int64, subject, body string) error {
	return enqueue(ctx, x, TopicNews, newsEvent{AffiliateID: affiliateID, Subject: subject, Body: body})
}

// DeriveNewsKey derives the unsubscribe-link key from the deployment secret.
func DeriveNewsKey(secret []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("partner-news-unsubscribe-v1"))
	return m.Sum(nil)
}

func newsMAC(key []byte, affiliateID int64) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("unsub:" + strconv.FormatInt(affiliateID, 10)))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// UnsubscribeToken is the signed token in a partner's unsubscribe link.
func UnsubscribeToken(key []byte, affiliateID int64) string {
	return strconv.FormatInt(affiliateID, 10) + "." + newsMAC(key, affiliateID)
}

// ParseUnsubscribeToken returns the partner a token was issued for.
func ParseUnsubscribeToken(key []byte, tok string) (int64, bool) {
	if len(key) == 0 {
		return 0, false
	}
	id, mac, ok := strings.Cut(tok, ".")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, hmac.Equal([]byte(mac), []byte(newsMAC(key, n)))
}

func (s *Sink) news(ctx context.Context, e newsEvent) error {
	var email, status string
	var name sql.NullString
	var optOut int
	err := s.DB.QueryRowContext(ctx, `SELECT email, name, status, news_opt_out FROM affiliates WHERE id = ?`, e.AffiliateID).
		Scan(&email, &name, &status, &optOut)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "active" || optOut != 0 || len(s.NewsKey) == 0 {
		return nil // paused, unsubscribed since, or no way to offer an unsubscribe link
	}
	prog, base := s.program(ctx)
	if base == "" {
		return errors.New("partnermail: no public URL to link to (set AE_PUBLIC_URL)")
	}
	unsub := base + "/api/v1/portal/unsubscribe?t=" + UnsubscribeToken(s.NewsKey, e.AffiliateID)
	// No automatic greeting: the owner writes their own message, often
	// starting "Hi!", and two greetings read badly.
	var b strings.Builder
	b.WriteString(strings.TrimSpace(e.Body) + "\n\n")
	b.WriteString("Your partner portal: " + base + "/\n\n")
	b.WriteString(s.programFooter(prog, unsub))
	return s.Mailer.Send(ctx, mail.Mail{To: email, Subject: e.Subject, Text: b.String(), FromName: prog, Tag: "program-news", ListUnsubscribe: unsub})
}
