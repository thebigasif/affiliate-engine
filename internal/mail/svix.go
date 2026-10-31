package mail

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Resend signs its webhooks with Svix (resend.com/docs/dashboard/
// webhooks/verify-webhooks-requests, docs.svix.com/receiving/
// verifying-payloads/how-manual):
//
//   - headers svix-id, svix-timestamp (Unix seconds) and svix-signature
//     (space-separated "v1,<base64 signature>" entries; webhook-* names
//     are accepted too);
//   - the signed content is id + "." + timestamp + "." + the raw body;
//   - the key is the signing secret without its "whsec_" prefix,
//     base64-decoded; the signature is base64(HMAC-SHA256(key, content));
//   - a timestamp more than SvixTolerance from now is refused (replay).

// SvixTolerance is how far a webhook's timestamp may be from now (the
// Svix libraries' default).
const SvixTolerance = 5 * time.Minute

// ErrBadSignature: the webhook is not signed with the secret, or is
// stale.
var ErrBadSignature = errors.New("mail: webhook signature does not verify")

// svixKey decodes a "whsec_..." signing secret.
func svixKey(secret string) ([]byte, error) {
	secret = strings.TrimSpace(secret)
	secret = strings.TrimPrefix(secret, "whsec_")
	if secret == "" {
		return nil, ErrBadSignature
	}
	key, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return nil, ErrBadSignature
	}
	return key, nil
}

// SvixSign returns the "v1,<signature>" for one message (tests and
// local tools that post signed events).
func SvixSign(secret, id string, ts time.Time, body []byte) (string, error) {
	key, err := svixKey(secret)
	if err != nil {
		return "", err
	}
	return "v1," + svixMAC(key, id, strconv.FormatInt(ts.Unix(), 10), body), nil
}

func svixMAC(key []byte, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySvix checks a Resend webhook's signature against the signing
// secret (AT_MAIL_WEBHOOK_SECRET, "whsec_..."). body is the raw request
// body, exactly as received.
func VerifySvix(secret string, h http.Header, body []byte, now time.Time) error {
	key, err := svixKey(secret)
	if err != nil {
		return err
	}
	get := func(name string) string {
		if v := h.Get("svix-" + name); v != "" {
			return v
		}
		return h.Get("webhook-" + name)
	}
	id, ts, sigs := get("id"), get("timestamp"), get("signature")
	if id == "" || ts == "" || sigs == "" {
		return ErrBadSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SvixTolerance || d < -SvixTolerance {
		return ErrBadSignature
	}
	want := []byte(svixMAC(key, id, ts, body))
	for _, s := range strings.Fields(sigs) {
		version, sig, ok := strings.Cut(s, ",")
		if !ok || version != "v1" {
			continue
		}
		if hmac.Equal([]byte(sig), want) {
			return nil
		}
	}
	return ErrBadSignature
}
