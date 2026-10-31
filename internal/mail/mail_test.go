package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromEnv(t *testing.T) {
	t.Setenv("AE_MAIL_PROVIDER", "")
	if c, err := FromEnv(); err != nil || New(c) != nil {
		t.Fatalf("unset provider: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"AE_MAIL_PROVIDER": "sendgrid"},
		{"AE_MAIL_PROVIDER": "resend", "AE_MAIL_FROM": "p@x.test"},                        // no key
		{"AE_MAIL_PROVIDER": "resend", "AE_MAIL_KEY": "re_1", "AE_MAIL_FROM": "not-mail"}, // bad from
		{"AE_MAIL_PROVIDER": "resend", "AE_MAIL_KEY": "re_1", "AE_MAIL_FROM": "p@x.test", "AE_MAIL_REPLY_TO": "nope"},
	} {
		for _, k := range []string{"AE_MAIL_PROVIDER", "AE_MAIL_KEY", "AE_MAIL_FROM", "AE_MAIL_REPLY_TO"} {
			t.Setenv(k, bad[k])
		}
		if _, err := FromEnv(); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	t.Setenv("AE_MAIL_PROVIDER", "Postmark")
	t.Setenv("AE_MAIL_KEY", "pm-token")
	t.Setenv("AE_MAIL_FROM", "partners@acme.test")
	t.Setenv("AE_MAIL_REPLY_TO", "")
	if c, err := FromEnv(); err != nil || c.Provider != "postmark" || New(c) == nil {
		t.Fatalf("postmark: %+v %v", c, err)
	}
}

// Both providers get the program name as the From display name, the key in
// their own auth header, and a newline in the name can't add a header.
func TestSend_Providers(t *testing.T) {
	var path, auth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization") + r.Header.Get("X-Postmark-Server-Token")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	m := Mail{To: "lee@x.test", Subject: "Hi", Text: "Body", FromName: "Acme\r\nBcc: evil@x.test", Tag: "t"}

	re := New(Config{Provider: "resend", Key: "re_1", From: "Anything <partners@acme.test>", ReplyTo: "owner@acme.test", Base: srv.URL})
	if err := re.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if path != "/emails" || auth != "Bearer re_1" || got["from"] != `"Acme  Bcc: evil@x.test" <partners@acme.test>` || got["reply_to"] != "owner@acme.test" {
		t.Fatalf("resend: %s %s %v", path, auth, got)
	}

	pm := New(Config{Provider: "postmark", Key: "pm_1", From: "partners@acme.test", Base: srv.URL})
	got = nil
	if err := pm.Send(context.Background(), Mail{To: "lee@x.test", Subject: "Hi", Text: "Body", FromName: "Acme Partners"}); err != nil {
		t.Fatal(err)
	}
	if path != "/email" || auth != "pm_1" || got["From"] != `"Acme Partners" <partners@acme.test>` || got["TextBody"] != "Body" {
		t.Fatalf("postmark: %s %s %v", path, auth, got)
	}
	if _, ok := got["ReplyTo"]; ok {
		t.Fatalf("empty reply-to sent: %v", got)
	}
	if err := pm.Send(context.Background(), Mail{To: "bad\r\n@x", Subject: "s"}); err == nil {
		t.Fatal("accepted a bad recipient")
	}
}

// A bulk email carries List-Unsubscribe in each provider's header format.
func TestSend_ListUnsubscribe(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		json.Unmarshal(b, &got)
	}))
	defer srv.Close()
	m := Mail{To: "a@x.test", Subject: "s", Text: "t", ListUnsubscribe: "https://p.test/u?t=1.ab"}
	New(Config{Provider: "resend", Key: "k", From: "p@x.test", Base: srv.URL}).Send(context.Background(), m)
	if h, _ := got["headers"].(map[string]any); h["List-Unsubscribe"] != "<https://p.test/u?t=1.ab>" {
		t.Fatalf("resend headers: %v", got["headers"])
	}
	New(Config{Provider: "postmark", Key: "k", From: "p@x.test", Base: srv.URL}).Send(context.Background(), m)
	hs, _ := got["Headers"].([]any)
	if len(hs) != 1 || hs[0].(map[string]any)["Value"] != "<https://p.test/u?t=1.ab>" {
		t.Fatalf("postmark headers: %v", got["Headers"])
	}
}

// RFC 8058: a one-click bulk email also carries List-Unsubscribe-Post.
func TestSend_ListUnsubscribeOneClick(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()
	m := Mail{To: "a@x.test", Subject: "s", Text: "t", ListUnsubscribe: "https://p.test/u?t=1.ab", OneClick: true}
	_ = New(Config{Provider: "resend", Key: "k", From: "p@x.test", Base: srv.URL}).Send(context.Background(), m)
	if h, _ := got["headers"].(map[string]any); h["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("resend headers: %v", got["headers"])
	}
	_ = New(Config{Provider: "postmark", Key: "k", From: "p@x.test", Base: srv.URL}).Send(context.Background(), m)
	hs, _ := got["Headers"].([]any)
	if len(hs) != 2 || hs[1].(map[string]any)["Name"] != "List-Unsubscribe-Post" || hs[1].(map[string]any)["Value"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("postmark headers: %v", got["Headers"])
	}
}

// FR-104: one email can go from a tenant's verified domain with its own
// Reply-To; a header-breaking override is refused before any request.
func TestSend_FromAndReplyToOverride(t *testing.T) {
	var got map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()
	cfg := Config{Provider: "postmark", Key: "k", From: "AffiliateTracking <mail@at.test>", ReplyTo: "help@at.test", Base: srv.URL}
	m := Mail{To: "a@x.test", Subject: "s", Text: "t", FromName: "Acme Partners", FromAddress: "affiliates@mail.acme.test", ReplyTo: "owner@acme.test"}
	if err := New(cfg).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got["From"] != `"Acme Partners" <affiliates@mail.acme.test>` || got["ReplyTo"] != "owner@acme.test" {
		t.Fatalf("postmark: %v", got)
	}
	// No display name: the configured one stays in front of the override.
	m.FromName = ""
	_ = New(cfg).Send(context.Background(), m)
	if got["From"] != `"AffiliateTracking" <affiliates@mail.acme.test>` {
		t.Fatalf("default name: %v", got["From"])
	}
	cfg.Provider = "resend"
	_ = New(cfg).Send(context.Background(), Mail{To: "a@x.test", Subject: "s", Text: "t", ReplyTo: "owner@acme.test"})
	if got["from"] != "AffiliateTracking <mail@at.test>" || got["reply_to"] != "owner@acme.test" {
		t.Fatalf("resend: %v", got)
	}
	before := calls
	for _, bad := range []Mail{
		{To: "a@x.test", Subject: "s", Text: "t", FromAddress: "x@y.test\r\nBcc: z@z.test"},
		{To: "a@x.test", Subject: "s", Text: "t", ReplyTo: "not an address"},
	} {
		if err := New(cfg).Send(context.Background(), bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if calls != before {
		t.Fatal("a refused override reached the provider")
	}
}

// Postmark: bulk mail names its broadcast stream (transactional mail
// stays on "outbound"), and metadata rides along for the webhooks.
func TestSend_PostmarkStreamAndMetadata(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()
	pm := New(Config{Provider: "postmark", Key: "k", From: "p@x.test", Base: srv.URL})
	if err := pm.Send(context.Background(), Mail{To: "a@x.test", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if got["MessageStream"] != "outbound" || got["Metadata"] != nil {
		t.Fatalf("default: %v", got)
	}
	if err := pm.Send(context.Background(), Mail{
		To: "a@x.test", Subject: "s", Text: "t", Stream: "broadcast",
		Metadata: map[string]string{"notice": "7"},
	}); err != nil {
		t.Fatal(err)
	}
	if md, _ := got["Metadata"].(map[string]any); got["MessageStream"] != "broadcast" || md["notice"] != "7" {
		t.Fatalf("broadcast: %v", got)
	}
}
