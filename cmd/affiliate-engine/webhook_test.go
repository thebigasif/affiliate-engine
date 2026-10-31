package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAdapter struct{ err error }

func (f fakeAdapter) HandleWebhook(context.Context, []byte, http.Header) error { return f.err }

// Adapter errors (signature detail, SQL errors) never reach the
// unauthenticated caller.
func TestWebhookHandler_GenericRejection(t *testing.T) {
	h := webhookHandler("test", fakeAdapter{err: errors.New("billing: invalid webhook signature: h1 mismatch; db=secret-table")})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/webhooks/test", strings.NewReader("{}")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "mismatch") || strings.Contains(body, "secret-table") {
		t.Fatalf("error detail leaked to caller: %q", body)
	}
	if strings.TrimSpace(body) != "rejected" {
		t.Fatalf("body=%q want rejected", body)
	}

	rec = httptest.NewRecorder()
	webhookHandler("test", fakeAdapter{})(rec, httptest.NewRequest("POST", "/webhooks/test", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("success code=%d", rec.Code)
	}
}

// An oversized body is rejected before the adapter runs.
func TestWebhookHandler_OversizedBody(t *testing.T) {
	called := false
	h := webhookHandler("test", adapterFunc(func() error { called = true; return nil }))
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/webhooks/test", strings.NewReader(strings.Repeat("x", (1<<20)+1))))
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("code=%d called=%v want 400/false", rec.Code, called)
	}
}

type adapterFunc func() error

func (f adapterFunc) HandleWebhook(context.Context, []byte, http.Header) error { return f() }
