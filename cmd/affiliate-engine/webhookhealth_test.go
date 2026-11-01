package main

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/billing"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/migrations"
)

// Accepted events and verified failures are counted per billing tool.
// A failed signature check is counted apart and never as an error; an
// unsigned request (a scanner, not Stripe) is not recorded at all.
func TestWebhookHealth_Recorded(t *testing.T) {
	h := dbtest.Open(t)
	webhookHealthDB = h.DB
	t.Cleanup(func() { webhookHealthDB = nil })

	post := func(hd hdr, err error) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhooks/stripe", strings.NewReader("{}"))
		for k, v := range hd {
			req.Header.Set(k, v)
		}
		webhookHandler("stripe", fakeAdapter{err: err})(httptest.NewRecorder(), req)
	}
	signed := hdr{"Stripe-Signature": "t=1,v1=x"}
	post(signed, nil)
	post(signed, nil)
	post(nil, billing.ErrInvalidSignature)                                     // scanner: not recorded
	post(signed, fmt.Errorf("%w: hmac mismatch", billing.ErrInvalidSignature)) // bad signature: counted apart
	post(signed, fmt.Errorf("%w: hmac mismatch", billing.ErrInvalidSignature)) // and again
	post(signed, fmt.Errorf("%w: missing id", billing.ErrMalformedEvent))      // verified, then failed
	post(signed, errors.New("store: database is locked"))                      // verified, then failed

	var okN, errN, sigN int
	var okAt, errAt, lastErr, sigAt string
	if err := h.DB.QueryRow(`SELECT ok_count, err_count, sigfail_count, last_ok_at, COALESCE(last_err_at, ''), COALESCE(last_err, ''), COALESCE(last_sigfail_at, '')
		FROM webhook_health WHERE provider = 'stripe'`).
		Scan(&okN, &errN, &sigN, &okAt, &errAt, &lastErr, &sigAt); err != nil {
		t.Fatal(err)
	}
	if okN != 2 || errN != 2 || sigN != 2 || okAt == "" || errAt == "" || sigAt == "" || !strings.Contains(lastErr, "database is locked") {
		t.Fatalf("ok=%d err=%d sig=%d okAt=%q errAt=%q sigAt=%q lastErr=%q", okN, errN, sigN, okAt, errAt, sigAt, lastErr)
	}
}

// Bad signatures alone never set the error fields the dashboard turns red on.
func TestWebhookHealth_SignatureFailuresNeverFailing(t *testing.T) {
	h := dbtest.Open(t)
	webhookHealthDB = h.DB
	t.Cleanup(func() { webhookHealthDB = nil })

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/webhooks/paddle", strings.NewReader("{}"))
		req.Header.Set("Paddle-Signature", "ts=1;h1=forged")
		webhookHandler("paddle", fakeAdapter{err: fmt.Errorf("%w: h1 mismatch", billing.ErrInvalidSignature)})(httptest.NewRecorder(), req)
	}
	var errN, sigN int
	var errAt string
	if err := h.DB.QueryRow(`SELECT err_count, sigfail_count, COALESCE(last_err_at, '') FROM webhook_health WHERE provider = 'paddle'`).
		Scan(&errN, &sigN, &errAt); err != nil {
		t.Fatal(err)
	}
	if errN != 0 || errAt != "" || sigN != 5 {
		t.Fatalf("err=%d errAt=%q sig=%d, want 0, empty, 5", errN, errAt, sigN)
	}
}

// The migration moves signature failures that older releases stored as
// errors over to the signature columns, and re-running it changes nothing.
func TestWebhookHealth_MigrationMovesOldSignatureErrors(t *testing.T) {
	h := dbtest.Open(t)
	if _, err := h.DB.Exec(`INSERT INTO webhook_health (provider, last_ok_at, ok_count, last_err_at, last_err, err_count) VALUES
		('stripe', '2026-09-30T10:00:00Z', 3, '2026-09-30T11:00:00Z', 'billing: invalid webhook signature: hmac mismatch', 1),
		('paddle', '2026-09-30T10:00:00Z', 3, '2026-09-30T11:00:00Z', 'store: database is locked', 1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrations.Apply(context.Background(), h.DB, dbtest.Engine()); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) (errAt, lastErr, sigAt string) {
		t.Helper()
		if err := h.DB.QueryRow(`SELECT COALESCE(last_err_at, ''), COALESCE(last_err, ''), COALESCE(last_sigfail_at, '') FROM webhook_health WHERE provider = ?`, p).
			Scan(&errAt, &lastErr, &sigAt); err != nil {
			t.Fatal(err)
		}
		return
	}
	if errAt, lastErr, sigAt := read("stripe"); errAt != "" || lastErr != "" || sigAt != "2026-09-30T11:00:00Z" {
		t.Fatalf("stripe: errAt=%q lastErr=%q sigAt=%q", errAt, lastErr, sigAt)
	}
	if errAt, lastErr, sigAt := read("paddle"); errAt != "2026-09-30T11:00:00Z" || lastErr != "store: database is locked" || sigAt != "" {
		t.Fatalf("paddle: errAt=%q lastErr=%q sigAt=%q", errAt, lastErr, sigAt)
	}
}

type hdr map[string]string
