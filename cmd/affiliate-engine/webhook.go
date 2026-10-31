package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/thebigasif/affiliate-engine/internal/billing"
)

// webhookHandler adapts a billing provider to its HTTP route.
//
// Read-error-safe bounded read: a read error (including the >1MiB limit
// trip) must reject, never feed a truncated body to the signature check.
//
// The caller is unauthenticated until the adapter says otherwise, so a
// failure answers only "rejected": signature-check details, parse errors
// and database errors are logged for the operator and never echoed.
func webhookHandler(provider string, a billing.Adapter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		if err := a.HandleWebhook(r.Context(), buf, r.Header); err != nil {
			log.Printf("webhook %s: rejected: %q", provider, err.Error()) // #nosec G706 -- %q escapes control characters: no log-line injection
			recordWebhook(r.Context(), provider, r.Header, err)
			http.Error(w, "rejected", http.StatusBadRequest)
			return
		}
		recordWebhook(r.Context(), provider, r.Header, nil)
		w.WriteHeader(http.StatusOK)
	}
}

// stripeAPIKey is the key the engine may call the Stripe API with:
// AE_STRIPE_API_KEY, else AE_STRIPE_SECRET_KEY when it holds an API key
// (sk_/rk_) rather than a webhook signing secret.
func stripeAPIKey() string {
	if k := strings.TrimSpace(os.Getenv("AE_STRIPE_API_KEY")); k != "" {
		return k
	}
	k := strings.TrimSpace(os.Getenv("AE_STRIPE_SECRET_KEY"))
	if strings.HasPrefix(k, "sk_") || strings.HasPrefix(k, "rk_") {
		return k
	}
	return ""
}
