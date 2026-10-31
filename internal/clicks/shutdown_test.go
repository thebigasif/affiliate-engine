package clicks_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
)

// waitAll fails the test if any accepted click's WaitGroup never fires:
// a stranded click hangs the redirect or bench that waits on it.
func waitAll(t *testing.T, wgs []*sync.WaitGroup) {
	t.Helper()
	all := make(chan struct{})
	go func() {
		for _, wg := range wgs {
			wg.Wait()
		}
		close(all)
	}()
	select {
	case <-all:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted clicks never completed: stranded in the queue")
	}
}

// The engine's committer runs on the signal context: SIGTERM cancels it
// while clicks wait in the buffer. They must still be written (with a
// context of their own), not dropped with the cancelled one.
func TestCommitter_CancelWritesBufferedClicks(t *testing.T) {
	h := newDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	c := clicks.NewCommitter(ctx, h.DB, 1000, time.Hour) // nothing flushes on its own
	t.Cleanup(c.Close)

	var wgs []*sync.WaitGroup
	for i := 0; i < 50; i++ {
		_, wg, err := c.Enqueue(1, 1, clicks.Click{IPHash: hash(byte(i)), Placement: "pl"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		wgs = append(wgs, wg)
	}
	cancel()
	waitAll(t, wgs)

	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Fatalf("clicks written after cancel = %d, want 50 (failed=%d)", n, c.Failed())
	}
}

// Redirects keep arriving during graceful shutdown. Every Enqueue must
// either be refused or be written; none may sit in a buffer nobody reads.
func TestCommitter_EnqueueDuringStopNeverStrands(t *testing.T) {
	for _, stop := range []string{"cancel", "close"} {
		t.Run(stop, func(t *testing.T) {
			h := newDB(t)
			ctx, cancel := context.WithCancel(context.Background())
			c := clicks.NewCommitter(ctx, h.DB, 8, 5*time.Millisecond)
			c.FullWait = 50 * time.Millisecond

			var (
				mu       sync.Mutex
				wgs      []*sync.WaitGroup
				accepted atomic.Int64
				wg       sync.WaitGroup
			)
			for p := 0; p < 8; p++ {
				wg.Add(1)
				go func(p int) {
					defer wg.Done()
					for i := 0; i < 200; i++ {
						_, w, err := c.Enqueue(1, 1, clicks.Click{IPHash: hash(byte(p)), Placement: "pl"}, time.Now())
						if err != nil {
							continue
						}
						accepted.Add(1)
						mu.Lock()
						wgs = append(wgs, w)
						mu.Unlock()
					}
				}(p)
			}
			time.Sleep(5 * time.Millisecond)
			if stop == "cancel" {
				cancel()
			} else {
				go c.Close()
			}
			wg.Wait()
			waitAll(t, wgs)
			cancel()
			c.Close()

			var n int64
			if err := h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if want := accepted.Load() - c.Failed(); n != want {
				t.Fatalf("clicks=%d, accepted=%d failed=%d", n, accepted.Load(), c.Failed())
			}
		})
	}
}
