package gamify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/thebigasif/affiliate-engine/internal/outbox"
)

// OutboxSink consumes commission.created events and drives XP/badges.
type OutboxSink struct {
	e  *Engine
	db *sql.DB
}

// NewOutboxSink builds one.
func NewOutboxSink(e *Engine, db *sql.DB) *OutboxSink {
	return &OutboxSink{e: e, db: db}
}

// Deliver implements outbox.Sink (at-least-once; grants dedupe internally).
func (s *OutboxSink) Deliver(ctx context.Context, ev outbox.Event) error {
	if ev.Topic != "commission.created" {
		return nil
	}
	var p struct {
		CommissionID int64 `json:"commission_id"`
		AffiliateID  int64 `json:"affiliate_id"`
		Amount       int64 `json:"amount"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("gamify sink: payload: %w", err)
	}
	if p.CommissionID == 0 || p.AffiliateID == 0 {
		return nil
	}
	if err := s.e.OnCommission(ctx, p.AffiliateID, p.CommissionID, p.Amount); err != nil {
		// Log-and-succeed: gamification must never block the money pipeline.
		log.Printf("gamify: commission %d: %v", p.CommissionID, err)
	}
	return nil
}
