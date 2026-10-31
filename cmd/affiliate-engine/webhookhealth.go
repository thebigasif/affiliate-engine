package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"
)

// webhookHealthDB is where webhook outcomes are recorded (set by main; nil
// in tests and CLI runs, which then record nothing).
var webhookHealthDB *sql.DB

// signedBy reports whether a request carries the provider's own proof of
// origin. Only those count as failures: a stray POST from a scanner has
// none and must not raise a "sales are failing" warning.
func signedBy(provider string, h http.Header) bool {
	switch provider {
	case "stripe":
		return h.Get("Stripe-Signature") != ""
	case "paddle":
		return h.Get("Paddle-Signature") != ""
	case "lemonsqueezy":
		return h.Get("X-Signature") != ""
	case "polar":
		return h.Get("webhook-signature") != ""
	case "chargebee":
		return h.Get("Authorization") != "" || h.Get("cb-signature") != ""
	case "recurly":
		return h.Get("Authorization") != "" || h.Get("recurly-signature") != ""
	}
	return false
}

// recordWebhook notes one webhook outcome for the dashboard's billing health.
// Best effort: a failure here is logged and never changes the response.
func recordWebhook(ctx context.Context, provider string, h http.Header, outcome error) {
	db := webhookHealthDB
	if db == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var err error
	if outcome == nil {
		_, err = db.ExecContext(ctx, `
			INSERT INTO webhook_health (provider, last_ok_at, ok_count) VALUES (?, ?, 1)
			ON CONFLICT(provider) DO UPDATE SET last_ok_at = excluded.last_ok_at, ok_count = webhook_health.ok_count + 1`,
			provider, now)
	} else if signedBy(provider, h) {
		msg := outcome.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		msg = strings.ToValidUTF8(msg, "?")
		_, err = db.ExecContext(ctx, `
			INSERT INTO webhook_health (provider, last_err_at, last_err, err_count) VALUES (?, ?, ?, 1)
			ON CONFLICT(provider) DO UPDATE SET last_err_at = excluded.last_err_at, last_err = excluded.last_err, err_count = webhook_health.err_count + 1`,
			provider, now, msg)
	}
	if err != nil {
		log.Printf("webhook %s: record health: %v", provider, err)
	}
}
