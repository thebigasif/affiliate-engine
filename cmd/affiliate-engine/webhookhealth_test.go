package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"
)

// Accepted events and signed rejections are counted per billing tool; an
// unsigned rejection (a scanner, not Stripe) is not.
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
	post(nil, errors.New("billing: invalid webhook signature"))                 // scanner: not counted
	post(signed, errors.New("billing: invalid webhook signature: v1 mismatch")) // real failure

	var okN, errN int
	var okAt, errAt, lastErr string
	if err := h.DB.QueryRow(`SELECT ok_count, err_count, last_ok_at, last_err_at, last_err FROM webhook_health WHERE provider = 'stripe'`).
		Scan(&okN, &errN, &okAt, &errAt, &lastErr); err != nil {
		t.Fatal(err)
	}
	if okN != 2 || errN != 1 || okAt == "" || errAt == "" || !strings.Contains(lastErr, "v1 mismatch") {
		t.Fatalf("ok=%d err=%d okAt=%q errAt=%q lastErr=%q", okN, errN, okAt, errAt, lastErr)
	}
}

type hdr map[string]string
