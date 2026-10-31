package outbox_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/dbtest"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/outbox"
)

func newDB(t *testing.T) *db.Handles {
	t.Helper()
	h := dbtest.Open(t)
	return h
}

type recorder struct {
	mu     sync.Mutex
	events []outbox.Event
	failOn map[string]bool // topic -> fail (for retry testing)
}

func (r *recorder) Deliver(_ context.Context, ev outbox.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn[ev.Topic] {
		return errSimulated
	}
	r.events = append(r.events, ev)
	return nil
}

// count returns the delivered count (race-safe).
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// topics returns delivered topics (race-safe).
func (r *recorder) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Topic)
	}
	return out
}

var errSimulated = &simErr{}

type simErr struct{}

func (*simErr) Error() string { return "simulated delivery failure" }

// O1: events are delivered and marked delivered exactly once.
func TestDispatcher_Delivers(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := h.DB.Exec(
			`INSERT INTO outbox_events (topic, payload) VALUES ('commission.created', ?)`,
			`{"n":`+string(rune('0'+i))+`}`); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recorder{}
	d := outbox.NewDispatcher(h.DB, 30*time.Millisecond, 10)
	d.Register(rec, "*")
	d.Start(ctx)
	t.Cleanup(d.Stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var undelivered int
		h.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE delivered_at IS NULL`).Scan(&undelivered)
		if undelivered == 0 && rec.count() == 3 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("delivered=%d/3", rec.count())
}

// O2: failed delivery retries with attempts++ and eventually succeeds.
func TestDispatcher_Retry(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	if _, err := h.DB.Exec(
		`INSERT INTO outbox_events (topic, payload) VALUES ('flaky.topic', '{}')`); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{failOn: map[string]bool{}}
	d := outbox.NewDispatcher(h.DB, 30*time.Millisecond, 10)

	// Fail twice, then succeed.
	failures := 0
	flaky := sinkFunc(func(ctx context.Context, ev outbox.Event) error {
		if failures < 2 {
			failures++
			return errSimulated
		}
		rec.Deliver(ctx, ev)
		return nil
	})
	d.Register(flaky, "flaky.topic")
	d.Start(ctx)
	t.Cleanup(d.Stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var attempts int
		var delivered sql.NullString
		h.DB.QueryRow(`SELECT attempts, delivered_at FROM outbox_events`).Scan(&attempts, &delivered)
		if delivered.Valid && attempts >= 2 {
			return // retried then delivered
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("event never delivered after retries")
}

// O3: topic routing — specific sink gets only its topic.
func TestDispatcher_Routing(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	h.DB.Exec(`INSERT INTO outbox_events (topic, payload) VALUES ('a.topic', '{}')`)
	h.DB.Exec(`INSERT INTO outbox_events (topic, payload) VALUES ('b.topic', '{}')`)
	onlyA := &recorder{}
	d := outbox.NewDispatcher(h.DB, 30*time.Millisecond, 10)
	d.Register(onlyA, "a.topic")
	d.Start(ctx)
	t.Cleanup(d.Stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tp := onlyA.topics()
		if len(tp) == 1 && tp[0] == "a.topic" {
			// b.topic has no sink and no fallback: it stays undelivered (not lost,
			// not error-looped — a.topic's delivery must not mark b).
			var bDelivered int
			h.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE topic='b.topic' AND delivered_at IS NOT NULL`).Scan(&bDelivered)
			if bDelivered == 0 {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("routing broken: got %v", onlyA.topics())
}

// sinkFunc adapts a function to Sink.
type sinkFunc func(ctx context.Context, ev outbox.Event) error

func (f sinkFunc) Deliver(ctx context.Context, ev outbox.Event) error { return f(ctx, ev) }
