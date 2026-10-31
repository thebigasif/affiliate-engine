package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// Resend tags accept only ASCII letters, numbers, underscores and
// dashes: the kind is cleaned ("affiliate.approved" would be a 422),
// Metadata rides as base64url tags that ResendMetadata reads back, and
// a broadcast stream sends from the broadcast address with a stream
// tag (Resend has no streams), keeping List-Unsubscribe.
func TestResendTagsMetadataAndBroadcast(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	defer srv.Close()
	cfg := Config{
		Provider: "resend", Key: "re_1", From: "AffiliateTracking <notices@mail.affiliatetracking.co>", Base: srv.URL,
		BroadcastFrom: "news@news.affiliatetracking.co",
	}
	meta := map[string]string{"at_shop": "gid://shop/548380009", "at_notice": "42"}

	if err := New(cfg).Send(context.Background(), Mail{To: "a@x.test", Subject: "s", Text: "t", Tag: "affiliate.approved", Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	if got["from"] != "\"AffiliateTracking\" <notices@mail.affiliatetracking.co>" && got["from"] != "AffiliateTracking <notices@mail.affiliatetracking.co>" {
		t.Fatalf("transactional from = %v", got["from"])
	}
	tags := map[string]string{}
	ok := regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	for _, x := range got["tags"].([]any) {
		m := x.(map[string]any)
		n, v := m["name"].(string), m["value"].(string)
		if !ok.MatchString(n) || !ok.MatchString(v) {
			t.Fatalf("tag %q=%q is not Resend-safe", n, v)
		}
		tags[n] = v
	}
	if tags["kind"] != "affiliate_approved" || tags["stream"] != "" {
		t.Fatalf("tags = %v", tags)
	}
	if back := ResendMetadata(tags); back["at_shop"] != meta["at_shop"] || back["at_notice"] != "42" || len(back) != 2 {
		t.Fatalf("metadata round trip = %v", back)
	}

	// Broadcast: the broadcast address keeps the display name.
	m := Mail{
		To: "a@x.test", Subject: "s", Text: "t", Tag: "bulk.news", Stream: "broadcast", FromName: "Acme Outdoors",
		ListUnsubscribe: "https://p.test/u?t=1", OneClick: true,
	}
	if err := New(cfg).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got["from"] != "\"Acme Outdoors\" <news@news.affiliatetracking.co>" {
		t.Fatalf("broadcast from = %v", got["from"])
	}
	if h, _ := got["headers"].(map[string]any); h["List-Unsubscribe"] != "<https://p.test/u?t=1>" || h["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("broadcast headers = %v", got["headers"])
	}
	found := false
	for _, x := range got["tags"].([]any) {
		if x.(map[string]any)["name"] == "stream" && x.(map[string]any)["value"] == "broadcast" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no stream tag: %v", got["tags"])
	}
	// A tenant's verified domain keeps its own address.
	m.FromAddress = "partners@mail.acme.test"
	_ = New(cfg).Send(context.Background(), m)
	if got["from"] != "\"Acme Outdoors\" <partners@mail.acme.test>" {
		t.Fatalf("tenant broadcast from = %v", got["from"])
	}
	// No broadcast address: the default sender.
	cfg.BroadcastFrom = ""
	m.FromAddress = ""
	_ = New(cfg).Send(context.Background(), m)
	if got["from"] != "\"Acme Outdoors\" <notices@mail.affiliatetracking.co>" {
		t.Fatalf("broadcast without its address = %v", got["from"])
	}
	// No kind, no metadata: no empty tags.
	_ = New(cfg).Send(context.Background(), Mail{To: "a@x.test", Subject: "s", Text: "t"})
	if _, has := got["tags"]; has {
		t.Fatalf("empty tags sent: %v", got["tags"])
	}
}

// Svix's published example (docs.svix.com/receiving/verifying-payloads/
// how-manual) verifies at its own timestamp; a changed body, a wrong
// secret, missing headers and a stale timestamp do not.
func TestVerifySvix(t *testing.T) {
	const secret = "whsec_plJ3nmyCDGBKInavdOK15jsl"
	body := []byte(`{"event_type":"ping","data":{"success":true}}`)
	at := time.Unix(1731705121, 0)
	h := http.Header{}
	h.Set("svix-id", "msg_loFOjxBNrRLzqYUf")
	h.Set("svix-timestamp", "1731705121")
	h.Set("svix-signature", "v1,rAvfW3dJ/X/qxhsaXPOyyCGmRKsaKWcsNccKXlIktD0=")
	if err := VerifySvix(secret, h, body, at.Add(time.Minute)); err != nil {
		t.Fatalf("svix example: %v", err)
	}
	// Several signatures (a rotated secret): any one may match.
	h.Set("svix-signature", "v1,bm9wZQ== v2,abc v1,rAvfW3dJ/X/qxhsaXPOyyCGmRKsaKWcsNccKXlIktD0=")
	if err := VerifySvix(secret, h, body, at); err != nil {
		t.Fatalf("multiple signatures: %v", err)
	}
	if err := VerifySvix(secret, h, []byte(`{"event_type":"ping","data":{"success":false}}`), at); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("changed body: %v", err)
	}
	if err := VerifySvix("whsec_"+"AAAAAAAAAAAAAAAAAAAAAAAA", h, body, at); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := VerifySvix(secret, h, body, at.Add(6*time.Minute)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("stale: %v", err)
	}
	if err := VerifySvix(secret, h, body, at.Add(-6*time.Minute)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("future: %v", err)
	}
	for _, drop := range []string{"svix-id", "svix-timestamp", "svix-signature"} {
		h2 := h.Clone()
		h2.Del(drop)
		if err := VerifySvix(secret, h2, body, at); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("without %s: %v", drop, err)
		}
	}
	if err := VerifySvix("", h, body, at); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("no secret: %v", err)
	}
	// SvixSign makes what VerifySvix accepts (webhook-* names too).
	now := time.Now()
	sig, err := SvixSign(secret, "msg_1", now, body)
	if err != nil {
		t.Fatal(err)
	}
	h3 := http.Header{}
	h3.Set("webhook-id", "msg_1")
	h3.Set("webhook-timestamp", strconv.FormatInt(now.Unix(), 10))
	h3.Set("webhook-signature", sig)
	if err := VerifySvix(secret, h3, body, now); err != nil {
		t.Fatalf("signed: %v", err)
	}
}
