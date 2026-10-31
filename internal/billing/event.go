// Package billing adapts payment-provider webhooks to the normalized
// commissions.Event pipeline.
//
// Security invariant (enforced by every implementation): the webhook
// signature is verified over the RAW request body BEFORE any database
// write, and the SAME raw bytes are then parsed. Replay protection lives
// downstream in commissions.Engine (raw_events unique on
// (source, external_event_id)); this package never writes money tables
// itself.
package billing

import (
	"context"
	"errors"
	"net/http"
)

// Adapter is the normalized webhook-ingest contract: take the raw body and
// headers, verify authenticity, map to a commissions.Event, and drive the
// engine. Returning a non-nil error tells the HTTP layer to NACK (400-class
// for signature failures — see ErrInvalidSignature — or 500-class to let the
// provider retry).
type Adapter interface {
	HandleWebhook(ctx context.Context, rawBody []byte, headers http.Header) error
}

var (
	// ErrInvalidSignature means the webhook failed HMAC verification (bad
	// secret, tampered body, missing/malformed header, or stale timestamp).
	// HTTP layers should map this to 400 and NOT retry.
	ErrInvalidSignature = errors.New("billing: invalid webhook signature")

	// ErrMalformedEvent means the signature was valid but the envelope did
	// not contain a parseable {id, type, data.object}. Map to 400.
	ErrMalformedEvent = errors.New("billing: malformed webhook event")
)
