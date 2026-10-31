// Package outbox delivers outbox_events at-least-once to sinks. Written in
// the same transaction as money mutations; delivery is decoupled and retried.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// MaxAttempts caps delivery attempts per event. An event that has failed
// this many times is dead-lettered: it stays in outbox_events (with
// last_error) for operator review but is no longer selected, so a
// permanently failing sink cannot burn the dispatcher forever.
const MaxAttempts = 10

// Sink consumes one event topic. Delivery is at-least-once; sinks MUST be
// idempotent (the event payload carries a stable event id for dedup).
type Sink interface {
	Deliver(ctx context.Context, ev Event) error
}

// Event is one outbox record.
type Event struct {
	ID        int64
	Topic     string
	Payload   json.RawMessage
	CreatedAt time.Time
	Attempts  int
}

// Dispatcher polls undelivered events and fans them to registered sinks.
type Dispatcher struct {
	db       *sql.DB
	sinks    map[string][]Sink // topic -> sinks ("*" matches all)
	fallback []Sink            // topics with no explicit sink
	interval time.Duration
	batch    int

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}
}

// NewDispatcher builds a dispatcher. interval defaults to 2s, batch to 100.
func NewDispatcher(db *sql.DB, interval time.Duration, batch int) *Dispatcher {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if batch <= 0 {
		batch = 100
	}
	return &Dispatcher{
		db:       db,
		sinks:    map[string][]Sink{},
		interval: interval,
		batch:    batch,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Register attaches a sink to topics; "*" registers for all topics as fallback.
func (d *Dispatcher) Register(sink Sink, topics ...string) {
	if len(topics) == 0 || (len(topics) == 1 && topics[0] == "*") {
		d.fallback = append(d.fallback, sink)
		return
	}
	for _, t := range topics {
		d.sinks[t] = append(d.sinks[t], sink)
	}
}

// Start launches the polling loop.
func (d *Dispatcher) Start(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()
	go d.run(ctx)
}

// Stop terminates the loop and waits for the in-flight batch.
func (d *Dispatcher) Stop() {
	close(d.stop)
	<-d.done
}

func (d *Dispatcher) run(ctx context.Context) {
	defer close(d.done)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stop:
			return
		case <-t.C:
			if err := d.deliverBatch(ctx); err != nil {
				log.Printf("outbox: batch error: %v", err)
			}
		}
	}
}

// deliverBatch claims undelivered events and delivers each.
//
// Selection: attempts < MaxAttempts (failed-out events are dead-lettered),
// ordered by attempts THEN id. Fresh events always sort ahead of ones that
// have already failed, so a poison event (or a whole batch of them) can no
// longer hold the head of the queue and starve everything behind it.
//
// Backoff note: outbox_events has no next_attempt_at column, so failed
// events are retried on the next tick after fresher work. Time-based
// exponential backoff needs that column (see the PR notes); an in-memory
// schedule would not survive restarts or multiple dispatchers.
func (d *Dispatcher) deliverBatch(ctx context.Context) error {
	// (SQLite-friendly single-writer flow; the delivered_at flag is the truth.)
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, topic, payload, created_at, attempts
		FROM outbox_events
		WHERE delivered_at IS NULL AND attempts < ?
		ORDER BY attempts, id
		LIMIT ?`, MaxAttempts, d.batch)
	if err != nil {
		return fmt.Errorf("outbox: query: %w", err)
	}
	var events []Event
	for rows.Next() {
		var e Event
		var created, payload string
		if err := rows.Scan(&e.ID, &e.Topic, &payload, &created, &e.Attempts); err != nil {
			_ = rows.Close()
			return err
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt, _ = time.Parse(time.RFC3339, created)
		events = append(events, e)
	}
	_ = rows.Close()
	if len(events) == 0 {
		return nil
	}

	for _, e := range events {
		sinks := d.sinksFor(e.Topic)
		if len(sinks) == 0 {
			// No subscriber yet: leave queued (attempts untouched) so a sink
			// registered later still receives it. Never drop, never fake-deliver.
			continue
		}
		var lastErr error
		ok := true
		for _, s := range sinks {
			if err := s.Deliver(ctx, e); err != nil {
				ok = false
				lastErr = err
			}
		}
		if ok {
			_, err := d.db.ExecContext(ctx, `
				UPDATE outbox_events SET delivered_at = ? WHERE id = ?`,
				time.Now().UTC().Format(time.RFC3339), e.ID)
			if err != nil {
				return fmt.Errorf("outbox: mark delivered %d: %w", e.ID, err)
			}
		} else {
			// attempts grows (reordering it behind fresh work); error kept
			// for the dead-letter view.
			if _, err := d.db.ExecContext(ctx, `
				UPDATE outbox_events SET attempts = attempts + 1, last_error = ? WHERE id = ?`,
				fmt.Sprint(lastErr), e.ID); err != nil {
				log.Printf("outbox: record failure %d: %v", e.ID, err)
			}
			if e.Attempts+1 >= MaxAttempts {
				log.Printf("outbox: event %d (%s) dead-lettered after %d attempts: %v",
					e.ID, e.Topic, e.Attempts+1, lastErr)
			}
		}
	}
	return nil
}

func (d *Dispatcher) sinksFor(topic string) []Sink {
	if s, ok := d.sinks[topic]; ok && len(s) > 0 {
		return s
	}
	return d.fallback
}

// LogSink logs events (dev/default sink).
type LogSink struct{}

// Deliver prints the event.
func (LogSink) Deliver(_ context.Context, ev Event) error {
	log.Printf("outbox[%s] #%d attempts=%d: %s", ev.Topic, ev.ID, ev.Attempts, truncate(ev.Payload, 200))
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
