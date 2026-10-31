package clicks_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/clicks"
)

// K6: one bad row (FK violation: unknown affiliate) no longer drops the
// whole batch — the batch is retried row by row, good clicks land, the
// bad one is counted.
func TestCommitter_BadRowDoesNotDropBatch(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	// Large flush interval + batch of 3: all three land in one batch.
	c := clicks.NewCommitter(ctx, h.DB, 3, time.Hour)
	var wgs []*sync.WaitGroup
	for _, aff := range []int64{1, 999, 1} {
		_, wg, err := c.Enqueue(aff, 1, clicks.Click{IPHash: hash(byte(aff))}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		wgs = append(wgs, wg)
	}
	for _, wg := range wgs {
		wg.Wait()
	}
	c.Close()

	var n int
	h.DB.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&n)
	if n != 2 {
		t.Fatalf("clicks=%d want 2 (good rows must survive a bad neighbour)", n)
	}
	var agg int
	h.DB.QueryRow(`SELECT COALESCE(SUM(clicks),0) FROM daily_agg`).Scan(&agg)
	if agg != 2 {
		t.Fatalf("daily_agg=%d want 2", agg)
	}
	if c.Failed() != 1 {
		t.Fatalf("failed=%d want 1", c.Failed())
	}
}

// K7: attacker-controlled dims are truncated before insert.
func TestCommitter_TruncatesLongFields(t *testing.T) {
	h := newDB(t)
	ctx := context.Background()
	c := clicks.NewCommitter(ctx, h.DB, 10, 10*time.Millisecond)
	t.Cleanup(c.Close)
	long := strings.Repeat("r", 5000)
	_, wg, err := c.Enqueue(1, 1, clicks.Click{
		Referer:   "https://x.dev/" + long,
		UTMSource: long, SubID: long, Placement: strings.Repeat("é", 300), // multi-byte
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var ref, utm, sub, pl string
	if err := h.DB.QueryRow(`SELECT referer, utm_source, subid, placement FROM clicks`).
		Scan(&ref, &utm, &sub, &pl); err != nil {
		t.Fatal(err)
	}
	if len(ref) != 1024 || len(utm) != 256 || len(sub) != 256 {
		t.Fatalf("lengths referer=%d utm=%d subid=%d", len(ref), len(utm), len(sub))
	}
	if len(pl) > 256 || !strings.HasPrefix(strings.Repeat("é", 128), pl) || len(pl)%2 != 0 {
		t.Fatalf("placement truncated mid-rune or too long: len=%d", len(pl))
	}
}

// blockingStore stalls every transaction until released — a writer that
// cannot keep up with the click rate.
type blockingStore struct{ release chan struct{} }

func (b *blockingStore) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("unused")
}

func (b *blockingStore) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func (b *blockingStore) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	<-b.release
	return nil, errors.New("store unavailable")
}

// K8: a full queue sheds clicks instead of stalling the redirect: Enqueue
// returns ErrQueueFull within ~FullWait and the drop is counted.
func TestCommitter_FullQueueDropsInsteadOfBlocking(t *testing.T) {
	st := &blockingStore{release: make(chan struct{})}
	c := clicks.NewCommitter(context.Background(), st, 1, time.Millisecond)
	c.FullWait = 5 * time.Millisecond

	start := time.Now()
	var full int
	for i := 0; i < 20; i++ {
		_, _, err := c.Enqueue(1, 1, clicks.Click{}, time.Now())
		if errors.Is(err, clicks.ErrQueueFull) {
			full++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	close(st.release)
	c.Close()

	if full == 0 || c.Dropped() != int64(full) {
		t.Fatalf("full=%d dropped=%d: saturated queue did not shed", full, c.Dropped())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("enqueue stalled %v behind a stuck writer", elapsed)
	}
}
