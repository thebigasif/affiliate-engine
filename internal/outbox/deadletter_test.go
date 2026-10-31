package outbox_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/outbox"
)

// O4: head-of-line — a poison event at the front of the queue must not
// starve fresh events behind it (batch size 1 makes the old ORDER BY id
// query re-pick the poison row forever).
func TestDispatcher_PoisonDoesNotBlockQueue(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO outbox_events (topic, payload) VALUES ('poison.topic', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO outbox_events (topic, payload) VALUES ('good.topic', '{}')`); err != nil {
		t.Fatal(err)
	}
	good := &recorder{}
	d := outbox.NewDispatcher(h.DB, 20*time.Millisecond, 1)
	d.Register(sinkFunc(func(context.Context, outbox.Event) error { return errSimulated }), "poison.topic")
	d.Register(good, "good.topic")
	d.Start(ctx)
	t.Cleanup(d.Stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if good.count() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fresh event starved behind a failing one")
}

// O5: attempts are capped — a permanently failing event is dead-lettered
// at MaxAttempts and never retried again (kept, with its last error).
func TestDispatcher_AttemptsCapped(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(`INSERT INTO outbox_events (topic, payload, attempts) VALUES ('dead.topic', '{}', ?)`,
		outbox.MaxAttempts-1); err != nil {
		t.Fatal(err)
	}
	var calls int64
	d := outbox.NewDispatcher(h.DB, 15*time.Millisecond, 10)
	d.Register(sinkFunc(func(context.Context, outbox.Event) error {
		atomic.AddInt64(&calls, 1)
		return errSimulated
	}), "dead.topic")
	d.Start(ctx)
	t.Cleanup(d.Stop)

	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("dead-lettered event delivered %d times, want exactly 1 final attempt", n)
	}
	var attempts int
	var lastErr string
	var delivered any
	if err := h.DB.QueryRow(`SELECT attempts, COALESCE(last_error,''), delivered_at FROM outbox_events`).
		Scan(&attempts, &lastErr, &delivered); err != nil {
		t.Fatal(err)
	}
	if attempts != outbox.MaxAttempts || lastErr == "" || delivered != nil {
		t.Fatalf("attempts=%d lastErr=%q delivered=%v", attempts, lastErr, delivered)
	}
}
