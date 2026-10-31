// Package clicks implements the measured group-commit click pipeline:
// redirect handlers append clicks to a channel; one committer goroutine owns
// the single write connection and flushes batches (≤N rows or T ms) in one
// transaction: raw inserts + coalesced daily_agg upsert + uniq_daily
// INSERT OR IGNORE. Benchmarked: +1.7µs/click total overhead.
package clicks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/ids"
)

// Stored-field caps. Referer and the traffic dims are attacker-controlled
// request data: bounded before insert so one crafted click cannot bloat
// rows (or fail the batch on a column limit in stricter engines).
const (
	maxRefererLen = 1024
	maxDimLen     = 256 // utm_source, subid, placement
)

// ErrQueueFull is returned by Enqueue when the buffer is saturated: the
// click is dropped (and counted) instead of stalling the redirect.
var ErrQueueFull = errors.New("clicks: queue full, click dropped")

// Click is one captured click, enriched at the edge.
type Click struct {
	IPHash      []byte // 16-byte canonical identity hash (nil for cookie-method)
	UAHash      []byte
	Referer     string
	LandingPath string
	UTMSource   string
	SubID       string
	Placement   string
	Country     string // ISO-3166 alpha-2 from the CDN geo header (no raw location stored)
	BotClass    int    // 0 human, 1 suspected, 2 known
	IsDC        bool   // datacenter IP (classified at ingest; raw IP never stored)
	RefCode     string // referral code the click came through ('' = numeric-id fallback)

	// ExtraParams: every redirect query param not claimed by a dedicated
	// column (FR-2's extra_json catch-all). Stored as
	// {"all_params":{...}} — the source for postback {ts:Source:param}
	// macros (FR-7): a click's custom traffic params ride along to the
	// conversion and out through the affiliate's postback template.
	ExtraParams map[string]string

	// Attribution: when non-zero, an attributions row is written in the SAME
	// transaction as the click (FK-safe, atomic window record).
	AttributionAffiliateID int64
	AttributionCampaignID  int64
	AttributionTTL         time.Duration
}

// ClickResult is returned to the caller after commit (async by default).
type ClickResult struct {
	ClickID     string
	Attribution struct {
		AffiliateID int64
		CampaignID  int64
	}
}

// Store is the persistence interface (satisfied by *sql.DB / *sql.Conn).
type Store interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Committer batches clicks and commits them in single transactions.
type Committer struct {
	store    Store
	maxBatch int
	flush    time.Duration

	ch        chan item
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// Stopping: Enqueue holds mu (read) while it hands a click over, and
	// the flusher takes it (write) once stopping is set, so every click
	// accepted before the stop is drained and written, and none lands in
	// a channel nobody reads.
	mu       sync.RWMutex
	stopping atomic.Bool

	// FullWait bounds how long Enqueue waits for buffer space before
	// dropping a click (0 = DefaultFullWait). It caps the redirect latency
	// a click flood can add. A negative value restores unbounded blocking
	// backpressure — for offline loaders and benchmarks only, never the
	// redirect path. Set it before the first Enqueue.
	FullWait time.Duration

	dropped atomic.Int64 // clicks shed because the queue was full
	failed  atomic.Int64 // clicks that could not be written even alone
}

// DefaultFullWait is the longest Enqueue waits on a full buffer before
// dropping the click. Under normal load the buffer (maxBatch*4) never
// fills; under a flood each redirect pays at most this, never an
// unbounded stall behind the single writer.
const DefaultFullWait = 250 * time.Millisecond

// drainTimeout bounds the final write at shutdown. It runs on a context
// of its own: the engine's context is the signal context, already
// cancelled by then, and would fail every pending click.
const drainTimeout = 10 * time.Second

// errClosed is returned by Enqueue once the committer is stopping.
var errClosed = fmt.Errorf("clicks: committer closed")

type item struct {
	affiliateID int64
	campaignID  int64
	click       Click
	ts          time.Time
	id          string
	wg          *sync.WaitGroup // signaled when committed
}

// NewCommitter starts the background flusher. Call Close to drain.
func NewCommitter(ctx context.Context, store Store, maxBatch int, flushEvery time.Duration) *Committer {
	if maxBatch <= 0 {
		maxBatch = 500
	}
	if flushEvery <= 0 {
		flushEvery = 150 * time.Millisecond
	}
	c := &Committer{
		store:    store,
		maxBatch: maxBatch,
		flush:    flushEvery,
		ch:       make(chan item, maxBatch*4),
		done:     make(chan struct{}),
	}
	c.wg.Add(1)
	go c.run(ctx)
	return c
}

// Enqueue registers a click without stalling the caller. When the buffer
// (maxBatch*4) is full it waits at most FullWait for space, then drops
// the click, counts it (Dropped) and returns ErrQueueFull: a click flood
// can never stall the redirect path, which only needs the id for the
// attribution token. The caller-supplied WaitGroup is signaled after the
// batch commits.
func (c *Committer) Enqueue(affiliateID, campaignID int64, cl Click, ts time.Time) (string, *sync.WaitGroup, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stopping.Load() {
		return "", nil, errClosed
	}
	id, err := ids.NewClickID()
	if err != nil {
		return "", nil, err
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	it := item{affiliateID: affiliateID, campaignID: campaignID, click: cl, ts: ts, id: id, wg: wg}
	select {
	case <-c.done:
		wg.Done()
		return "", nil, errClosed
	case c.ch <- it:
		return id, wg, nil
	default:
	}
	wait := c.FullWait
	if wait < 0 {
		select { // legacy backpressure (offline callers only)
		case c.ch <- it:
			return id, wg, nil
		case <-c.done:
			wg.Done()
			return "", nil, fmt.Errorf("clicks: committer closed")
		}
	}
	if wait == 0 {
		wait = DefaultFullWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case c.ch <- it:
		return id, wg, nil
	case <-c.done:
		wg.Done()
		return "", nil, errClosed
	case <-timer.C:
		wg.Done()
		if n := c.dropped.Add(1); n == 1 || n%1000 == 0 {
			log.Printf("clicks: queue full, dropped %d click(s) so far", n)
		}
		return "", nil, ErrQueueFull
	}
}

// Dropped reports how many clicks were shed because the queue was full.
func (c *Committer) Dropped() int64 { return c.dropped.Load() }

// Failed reports how many clicks could not be written even when retried
// on their own.
func (c *Committer) Failed() int64 { return c.failed.Load() }

// Close drains pending items and stops the flusher. Safe to call twice.
func (c *Committer) Close() {
	c.closeOnce.Do(func() { close(c.done) })
	c.wg.Wait()
}

func (c *Committer) run(ctx context.Context) {
	defer c.wg.Done()
	tick := time.NewTicker(c.flush)
	defer tick.Stop()
	var batch []item
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.commitWithFallback(ctx, batch)
		for _, it := range batch {
			it.wg.Done()
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			c.drain(ctx, batch)
			return
		case <-c.done:
			c.drain(ctx, batch)
			return
		case it := <-c.ch:
			batch = append(batch, it)
			if len(batch) >= c.maxBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// drain stops intake and writes every click already accepted: the batch
// in hand, the buffer, and clicks from Enqueue calls still in flight
// (it keeps reading the channel until they have all returned, so a
// sender blocked on a full buffer is never left waiting on it).
func (c *Committer) drain(ctx context.Context, batch []item) {
	c.stopping.Store(true)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.commitWithFallback(fctx, batch)
		for _, it := range batch {
			it.wg.Done()
		}
		batch = batch[:0]
	}
	locked := make(chan struct{})
	go func() {
		c.mu.Lock() // every in-flight Enqueue has returned
		close(locked)
	}()
	for {
		select {
		case it := <-c.ch:
			batch = append(batch, it)
			if len(batch) >= c.maxBatch {
				flush()
			}
		case <-locked:
			for {
				select {
				case it := <-c.ch:
					batch = append(batch, it)
				default:
					flush()
					c.mu.Unlock() // later Enqueues see stopping and return
					return
				}
			}
		}
	}
}

// commitWithFallback commits the batch in one transaction; if that fails
// (one bad row aborts the whole tx), every row is retried in its own
// transaction so the good clicks still land. Rows that fail alone are
// logged and counted, never silently lost with their neighbours.
func (c *Committer) commitWithFallback(ctx context.Context, batch []item) {
	err := c.commitBatch(ctx, batch)
	if err == nil {
		return
	}
	if len(batch) == 1 {
		c.failed.Add(1)
		log.Printf("clicks: dropped click %s (affiliate=%d): %v", batch[0].id, batch[0].affiliateID, err)
		return
	}
	log.Printf("clicks: batch of %d failed (%v); retrying rows individually", len(batch), err)
	for i := range batch {
		if rerr := c.commitBatch(ctx, batch[i:i+1]); rerr != nil {
			c.failed.Add(1)
			log.Printf("clicks: dropped click %s (affiliate=%d): %v", batch[i].id, batch[i].affiliateID, rerr)
		}
	}
}

// truncate bounds s to n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && s[i]&0xC0 == 0x80 { // back off continuation bytes
		i--
	}
	return s[:i]
}

// commitBatch writes one transaction: clicks + daily_agg upsert + uniq_daily.
// Patterns proven in research/data-layer/analytics.md §2 (write path) and
// sqlite-ops.md §2 (staged batch keys, single coalesced upsert).
func (c *Committer) commitBatch(ctx context.Context, batch []item) error {
	tx, err := begin(ctx, c.store)
	if err != nil {
		return fmt.Errorf("clicks: begin: %w", err)
	}
	defer tx.Rollback()

	// Per-item stats to coalesce (day, aff, camp) -> clicks.
	type key struct {
		day       int64
		aff, camp int64
	}
	aggClicks := map[key]int{}
	dcAgg := map[key]int{}

	for _, it := range batch {
		day, _ := parseDay(it.ts)
		month := day / 100
		exclude := 0
		if it.click.BotClass == 2 {
			exclude = 1 // known bots excluded from stats (still stored raw)
		}
		extraJSON := "{}"
		if len(it.click.ExtraParams) > 0 {
			if b, err := json.Marshal(map[string]any{"all_params": it.click.ExtraParams}); err == nil {
				extraJSON = string(b)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO clicks (id, ts, day, month, affiliate_id, campaign_id,
			                    ip_hash, ua_hash, referer, landing_path,
			                    utm_source, subid, placement, country, bot_class, exclude_stats, extra_json, ref_code)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			it.id, it.ts.UTC().Format(time.RFC3339Nano), day, month,
			it.affiliateID, it.campaignID,
			it.click.IPHash, it.click.UAHash, truncate(it.click.Referer, maxRefererLen), it.click.LandingPath,
			truncate(it.click.UTMSource, maxDimLen), truncate(it.click.SubID, maxDimLen),
			truncate(it.click.Placement, maxDimLen), it.click.Country, it.click.BotClass, exclude, extraJSON,
			truncate(it.click.RefCode, maxDimLen),
		); err != nil {
			return fmt.Errorf("clicks: insert %s: %w", it.id, err)
		}
		if it.click.IPHash != nil && exclude == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO uniq_daily (day, affiliate_id, ip_hash)
				VALUES (?, ?, ?)
				ON CONFLICT DO NOTHING`, day, it.affiliateID, it.click.IPHash); err != nil {
				return fmt.Errorf("clicks: uniq %s: %w", it.id, err)
			}
		}
		if exclude == 0 {
			aggClicks[key{day, it.affiliateID, it.campaignID}]++
			if it.click.IsDC {
				dcAgg[key{day, it.affiliateID, it.campaignID}]++
			}
		}
		// Attribution row in the same tx (FK: click row exists now).
		if it.click.AttributionAffiliateID != 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO attributions (click_id, affiliate_id, campaign_id, method, created_at, expires_at)
				VALUES (?, ?, ?, 'token', ?, ?)`,
				it.id, it.click.AttributionAffiliateID, it.click.AttributionCampaignID,
				it.ts.UTC().Format(time.RFC3339),
				it.ts.UTC().Add(it.click.AttributionTTL).Format(time.RFC3339)); err != nil {
				return fmt.Errorf("clicks: attribution %s: %w", it.id, err)
			}
		}
	}

	// Datacenter counters (per day+aff; campaign folded in — the rule is
	// affiliate-scoped).
	type dkey struct{ day, aff int64 }
	dcDay := map[dkey][2]int64{} // [dc, total]
	for k, n := range aggClicks {
		e := dcDay[dkey{k.day, k.aff}]
		e[1] += int64(n)
		e[0] += int64(dcAgg[k])
		dcDay[dkey{k.day, k.aff}] = e
	}
	for k, v := range dcDay {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dc_click_daily (day, affiliate_id, dc_clicks, total_clicks)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(day, affiliate_id)
			DO UPDATE SET dc_clicks = dc_click_daily.dc_clicks + ?, total_clicks = dc_click_daily.total_clicks + ?`,
			k.day, k.aff, v[0], v[1], v[0], v[1]); err != nil {
			return fmt.Errorf("clicks: dc agg: %w", err)
		}
	}

	// One coalesced upsert per (day, aff, camp) — never per row.
	for k, n := range aggClicks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO daily_agg (day, affiliate_id, campaign_id, clicks)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(day, affiliate_id, campaign_id, currency)
			DO UPDATE SET clicks = daily_agg.clicks + ?`,
			k.day, k.aff, k.camp, n, n); err != nil {
			return fmt.Errorf("clicks: agg upsert: %w", err)
		}
	}

	return tx.Commit()
}

// --- tiny helpers (avoid extra deps) ---

type txLike interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}

// begin starts a transaction on either *sql.DB or *sql.Conn.
type dbStarter interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func begin(ctx context.Context, s Store) (txLike, error) {
	if d, ok := s.(dbStarter); ok {
		return d.BeginTx(ctx, nil)
	}
	// Conn-based store: BEGIN IMMEDIATE via exec then reuse — modernc supports
	// explicit BEGIN on a Conn only through its own Tx; fallback: single-statement
	// path per batch is used by tests via *sql.DB.
	return nil, fmt.Errorf("clicks: store does not support BeginTx")
}

func parseDay(t time.Time) (int64, error) {
	var n int64
	for _, c := range t.UTC().Format("20060102") {
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
