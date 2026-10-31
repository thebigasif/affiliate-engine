// Package mail sends one plain-text email through Postmark or Resend,
// and verifies Resend's signed webhooks (svix.go).
// The engine uses it for partner emails (login links, payouts); the
// provider is the operator's own account (AE_MAIL_*).
package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"sort"
	"strings"
	"time"
)

// Mail is one plain-text email.
type Mail struct {
	To, Subject, Text string
	FromName          string // display name for this email; "" = the configured one
	// FromAddress replaces the configured sending address for this email
	// (a tenant's verified sending domain, FR-104); "" = the configured one.
	FromAddress string
	// ReplyTo replaces the configured Reply-To for this email; "" = the
	// configured one.
	ReplyTo         string
	Tag             string // what kind of email (provider analytics)
	ListUnsubscribe string // one-click unsubscribe URL (bulk emails only)
	// OneClick adds List-Unsubscribe-Post (RFC 8058); set it only when
	// ListUnsubscribe accepts a POST that unsubscribes with no page.
	OneClick bool
	// Stream is the Postmark message stream: "" = "outbound" (the
	// transactional stream). Bulk or marketing mail must name a
	// broadcast stream. Resend has no streams: a non-transactional
	// stream sends from Config.BroadcastFrom when it is set (a separate
	// subdomain keeps its own reputation) and is tagged stream=<name>.
	Stream string
	// Metadata is echoed back by the provider's bounce and spam
	// complaint webhooks, so a report finds its email: Postmark
	// Metadata, or Resend tags (see ResendMetadata).
	Metadata map[string]string
}

// Mailer sends one email.
type Mailer interface {
	Send(ctx context.Context, m Mail) error
}

// Config picks and authenticates the provider.
type Config struct {
	Provider string // postmark | resend | log
	Key      string // provider API key / server token
	From     string // "Name <addr>" or a bare address
	ReplyTo  string // optional
	Base     string // provider API base (tests override)
	// BroadcastFrom is the sending address for broadcast-stream mail
	// (Resend; "" = From). Mail that names its own FromAddress (a
	// tenant's verified domain) keeps it.
	BroadcastFrom string
}

// FromEnv reads AE_MAIL_PROVIDER, AE_MAIL_KEY, AE_MAIL_FROM and
// AE_MAIL_REPLY_TO. An empty provider means email is off (zero Config).
func FromEnv() (Config, error) {
	c := Config{
		Provider: strings.ToLower(strings.TrimSpace(os.Getenv("AE_MAIL_PROVIDER"))),
		Key:      strings.TrimSpace(os.Getenv("AE_MAIL_KEY")),
		From:     strings.TrimSpace(os.Getenv("AE_MAIL_FROM")),
		ReplyTo:  strings.TrimSpace(os.Getenv("AE_MAIL_REPLY_TO")),
		Base:     strings.TrimSpace(os.Getenv("AE_MAIL_BASE")),
	}
	switch c.Provider {
	case "":
		return Config{}, nil
	case "log":
		return c, nil
	case "postmark", "resend":
		if c.Key == "" {
			return c, fmt.Errorf("mail: AE_MAIL_PROVIDER=%s needs AE_MAIL_KEY", c.Provider)
		}
		if _, err := mail.ParseAddress(c.From); err != nil {
			return c, fmt.Errorf("mail: AE_MAIL_FROM must be an address the provider has verified, like partners@yourdomain.com")
		}
		if c.ReplyTo != "" {
			if _, err := mail.ParseAddress(c.ReplyTo); err != nil {
				return c, fmt.Errorf("mail: AE_MAIL_REPLY_TO is not an email address")
			}
		}
		return c, nil
	}
	return c, fmt.Errorf("mail: AE_MAIL_PROVIDER must be postmark, resend or log (got %q)", c.Provider)
}

// New returns the configured mailer; nil = email is off.
func New(c Config) Mailer {
	switch c.Provider {
	case "postmark":
		return &apiMailer{cfg: c, base: or(c.Base, "https://api.postmarkapp.com")}
	case "resend":
		return &apiMailer{cfg: c, base: or(c.Base, "https://api.resend.com")}
	case "log":
		return logMailer{}
	}
	return nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// logMailer writes emails to the log instead of sending (local testing).
type logMailer struct{}

func (logMailer) Send(_ context.Context, m Mail) error {
	log.Printf("mail (log only) to=%q subject=%q\n%s", m.To, m.Subject, m.Text) // #nosec G706 -- %q escapes the header fields; the body is our own template
	return nil
}

type apiMailer struct {
	cfg  Config
	base string
}

var client = &http.Client{Timeout: 15 * time.Second}

// from renders the From header: the per-email display name (the program's
// name) in front of the configured address.
func (a *apiMailer) from(name, override string) string {
	from := a.cfg.From
	if override != "" {
		from = override
	}
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return from
	}
	if name == "" {
		if override == "" {
			return from
		}
		if cfgAddr, err := mail.ParseAddress(a.cfg.From); err == nil {
			name = cfgAddr.Name
		}
	}
	return (&mail.Address{Name: name, Address: addr.Address}).String()
}

func (a *apiMailer) Send(ctx context.Context, m Mail) error {
	if _, err := mail.ParseAddress(m.To); err != nil || strings.ContainsAny(m.To, "\r\n") {
		return fmt.Errorf("mail: bad recipient")
	}
	name := strings.NewReplacer("\r", " ", "\n", " ").Replace(m.FromName)
	for _, v := range []string{m.FromAddress, m.ReplyTo} {
		if v == "" {
			continue
		}
		if _, err := mail.ParseAddress(v); err != nil || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("mail: bad sender address")
		}
	}
	replyTo := a.cfg.ReplyTo
	if m.ReplyTo != "" {
		replyTo = m.ReplyTo
	}
	var body any
	var path string
	hdr := map[string]string{}
	switch a.cfg.Provider {
	case "postmark":
		path = "/email"
		hdr["X-Postmark-Server-Token"] = a.cfg.Key
		b := map[string]any{
			"From": a.from(name, m.FromAddress), "To": m.To, "Subject": m.Subject,
			"TextBody": m.Text, "Tag": m.Tag, "MessageStream": or(m.Stream, "outbound"),
		}
		if len(m.Metadata) > 0 {
			b["Metadata"] = m.Metadata
		}
		if m.ListUnsubscribe != "" {
			hs := []map[string]string{{"Name": "List-Unsubscribe", "Value": "<" + m.ListUnsubscribe + ">"}}
			if m.OneClick {
				hs = append(hs, map[string]string{"Name": "List-Unsubscribe-Post", "Value": "List-Unsubscribe=One-Click"})
			}
			b["Headers"] = hs
		}
		if replyTo != "" {
			b["ReplyTo"] = replyTo
		}
		body = b
	default: // resend
		path = "/emails"
		hdr["Authorization"] = "Bearer " + a.cfg.Key
		fromAddr := m.FromAddress
		broadcast := m.Stream != "" && m.Stream != "outbound"
		if broadcast && fromAddr == "" && a.cfg.BroadcastFrom != "" {
			if addr, err := mail.ParseAddress(a.cfg.BroadcastFrom); err == nil {
				fromAddr = addr.Address
			}
		}
		b := map[string]any{
			"from": a.from(name, fromAddr), "to": []string{m.To}, "subject": m.Subject,
			"text": m.Text,
		}
		if tags := resendTags(m, broadcast); len(tags) > 0 {
			b["tags"] = tags
		}
		if m.ListUnsubscribe != "" {
			hs := map[string]string{"List-Unsubscribe": "<" + m.ListUnsubscribe + ">"}
			if m.OneClick {
				hs["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
			}
			b["headers"] = hs
		}
		if replyTo != "" {
			b["reply_to"] = replyTo
		}
		body = b
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) // #nosec G704 -- fixed provider API host from operator config
	if err != nil {
		return fmt.Errorf("mail via %s: %w", a.cfg.Provider, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mail via %s: HTTP %d: %s", a.cfg.Provider, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// resendTagSafe keeps what Resend accepts in a tag name or value: ASCII
// letters, numbers, underscores and dashes, at most 256 characters.
// Anything else becomes an underscore ("affiliate.approved" is sent as
// "affiliate_approved").
func resendTagSafe(v string) string {
	var sb strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	out := sb.String()
	if len(out) > 256 {
		out = out[:256]
	}
	return out
}

// resendMetaPrefix marks a tag that carries one Metadata entry; its
// value is base64url (no padding) so any string survives Resend's tag
// character set. ResendMetadata reverses it.
const resendMetaPrefix = "m_"

func resendTags(m Mail, broadcast bool) []map[string]string {
	var tags []map[string]string
	if v := resendTagSafe(m.Tag); v != "" {
		tags = append(tags, map[string]string{"name": "kind", "value": v})
	}
	if broadcast {
		tags = append(tags, map[string]string{"name": "stream", "value": resendTagSafe(m.Stream)})
	}
	keys := make([]string, 0, len(m.Metadata))
	for k := range m.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := base64.RawURLEncoding.EncodeToString([]byte(m.Metadata[k]))
		if len(v) > 256 {
			continue
		}
		tags = append(tags, map[string]string{"name": resendTagSafe(resendMetaPrefix + k), "value": v})
	}
	return tags
}

// ResendMetadata returns the Metadata an email was sent with from the
// tags a Resend webhook echoes back (data.tags, an object of name to
// value). Tags that are not Metadata are skipped.
func ResendMetadata(tags map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range tags {
		if !strings.HasPrefix(k, resendMetaPrefix) {
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(k, resendMetaPrefix)] = string(b)
	}
	return out
}
