package ledger_test

// The money property tests. Every mutation path must keep all of these
// green on BOTH engines (CI runs the suite per engine).
// Contract: research/data-layer/ledger.md §8 (15 tests).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/db"
	"github.com/thebigasif/affiliate-engine/internal/dbtest"
	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

func nowUTC() time.Time { return time.Now().UTC() }

func newTestDB(t *testing.T) (*db.Handles, *ledger.Poster) {
	t.Helper()
	ctx := context.Background()
	// Dual-engine: file-backed SQLite by default; Postgres when
	// AE_TEST_ENGINES=postgres + AE_TEST_PG_ADMIN_DSN are set (CI).
	h := dbtest.Open(t)
	p := ledger.NewPoster(h.Ledger)
	if err := p.EnsureAccounts(ctx, map[string]string{
		"HOLD:USD":                "liability",
		"EXPENSE:COMMISSION:USD":  "expense",
		"AFFILIATE_PAYABLE:1:USD": "liability",
		"PAYOUT_CLEARING:USD":     "clearing",
	}); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	seedAffiliate(t, h.DB, 1)
	return h, p
}

func seedAffiliate(t *testing.T, d *sql.DB, id int64) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO affiliates (id, email) VALUES (?, ?)`,
		id, fmt.Sprintf("aff%d@test.dev", id)); err != nil {
		t.Fatalf("seed affiliate: %v", err)
	}
}

func journalCount(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM journal_entries`).Scan(&n); err != nil {
		t.Fatalf("count journals: %v", err)
	}
	return n
}

func entrySum(t *testing.T, d *sql.DB) int64 {
	t.Helper()
	var s int64
	if err := d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM ledger_entries`).Scan(&s); err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	return s
}

func lines(amount int64) []ledger.Line {
	return []ledger.Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: amount},
		{Account: "HOLD:USD", Amount: -amount},
	}
}

// P1: replaying the same idem key twice yields exactly one journal.
func TestProperty_1_ReplayYieldsExactlyOne(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	j1, err := p.Post(ctx, "evt-1", "conversion", "", nowUTC(), lines(1000))
	if err != nil {
		t.Fatal(err)
	}
	j2, err := p.Post(ctx, "evt-1", "conversion", "", nowUTC(), lines(1000))
	if err != ledger.ErrDuplicate {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if j1 != j2 {
		t.Fatalf("replay returned different journal: %d vs %d", j1, j2)
	}
	if n := journalCount(t, h.DB); n != 1 {
		t.Fatalf("journals=%d, want 1", n)
	}
}

// P2: unbalanced journal rejected (sum != 0).
func TestProperty_2_UnbalancedRejected(t *testing.T) {
	_, p := newTestDB(t)
	_, err := p.Post(context.Background(), "evt-unb", "conversion", "", nowUTC(),
		[]ledger.Line{{Account: "EXPENSE:COMMISSION:USD", Amount: 500}, {Account: "HOLD:USD", Amount: -400}})
	if err == nil {
		t.Fatal("unbalanced journal accepted")
	}
}

// P3: single-line journal rejected.
func TestProperty_3_SingleLineRejected(t *testing.T) {
	_, p := newTestDB(t)
	_, err := p.Post(context.Background(), "evt-1line", "conversion", "", nowUTC(),
		[]ledger.Line{{Account: "HOLD:USD", Amount: 0}})
	if err == nil {
		t.Fatal("single-line journal accepted")
	}
}

// P4: unknown account rejected.
func TestProperty_4_UnknownAccountRejected(t *testing.T) {
	_, p := newTestDB(t)
	_, err := p.Post(context.Background(), "evt-unknown", "conversion", "", nowUTC(),
		[]ledger.Line{{Account: "NOPE", Amount: 1}, {Account: "HOLD:USD", Amount: -1}})
	if err == nil {
		t.Fatal("unknown account accepted")
	}
}

// P5: ledger stays globally balanced after N posts (integrity invariant).
func TestProperty_5_GlobalSumZero(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := p.Post(ctx, fmt.Sprintf("evt-%d", i), "conversion", "", nowUTC(), lines(int64(100+i))); err != nil {
			t.Fatal(err)
		}
	}
	if s := entrySum(t, h.DB); s != 0 {
		t.Fatalf("global sum=%d, want 0", s)
	}
}

// P6: affiliate payable can never go negative (overdraft guard).
func TestProperty_6_NoOverdraft(t *testing.T) {
	_, p := newTestDB(t)
	ctx := context.Background()
	// Credit 1000 to payable via HOLD, then try to debit 2000.
	if _, err := p.Post(ctx, "pay-credit", "conversion", "", nowUTC(), []ledger.Line{
		{Account: "HOLD:USD", Amount: 1000},
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: -1000},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := p.Post(ctx, "pay-debit", "payout", "", nowUTC(), []ledger.Line{
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: 2000},
		{Account: "PAYOUT_CLEARING:USD", Amount: -2000},
	})
	if err == nil {
		t.Fatal("overdraft accepted")
	}
}

// P7: concurrent same-idem posts — exactly one commits (racing goroutines on
// separate pool connections; busy_timeout + unique key arbitrate; SQLITE_BUSY
// from lock contention is retried because it is transient, NOT a duplicate).
func TestProperty_7_ConcurrentDuplicate(t *testing.T) {
	h, _ := newTestDB(t)
	ctx := context.Background()
	const workers = 4
	done := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			c, err := h.DB.Conn(ctx)
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			p := ledger.NewPoster(c)
			var err2 error
			for attempt := 0; attempt < 20; attempt++ {
				_, err2 = p.Post(ctx, "evt-race", "conversion", "", nowUTC(), lines(777))
				if err2 == nil || err2 == ledger.ErrDuplicate {
					break
				}
				if !strings.Contains(err2.Error(), "locked") && !strings.Contains(err2.Error(), "busy") {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			done <- err2
		}()
	}
	var dup, ok int
	for i := 0; i < workers; i++ {
		if err := <-done; err == ledger.ErrDuplicate {
			dup++
		} else if err == nil {
			ok++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("committed=%d, want exactly 1 (dup=%d)", ok, dup)
	}
	if n := journalCount(t, h.DB); n != 1 {
		t.Fatalf("journals=%d, want 1", n)
	}
}

// P8: balance derivation — AccountBalance equals posted amounts.
func TestProperty_8_DerivedBalance(t *testing.T) {
	_, p := newTestDB(t)
	ctx := context.Background()
	if _, err := p.Post(ctx, "b1", "conversion", "", nowUTC(), []ledger.Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 5000},
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: -5000},
	}); err != nil {
		t.Fatal(err)
	}
	bal, err := p.AccountBalance(ctx, "AFFILIATE_PAYABLE:1:USD")
	if err != nil {
		t.Fatal(err)
	}
	if bal != -5000 {
		t.Fatalf("balance=%d, want -5000 (liability credit)", bal)
	}
}

// P9: schema-level immutability — direct UPDATE/DELETE on ledger rows aborted.
func TestProperty_9_LedgerImmutable(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	if _, err := p.Post(ctx, "imm", "conversion", "", nowUTC(), lines(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE ledger_entries SET amount = 999 WHERE id = 1`); err == nil {
		t.Fatal("UPDATE on ledger_entries accepted")
	}
	if _, err := h.DB.Exec(`DELETE FROM ledger_entries WHERE id = 1`); err == nil {
		t.Fatal("DELETE on ledger_entries accepted")
	}
	if _, err := h.DB.Exec(`DELETE FROM journal_entries WHERE id = 1`); err == nil {
		t.Fatal("DELETE on journal_entries accepted")
	}
}

// P10: commission state machine — only legal transitions.
func TestProperty_10_CommissionStateMachine(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	jid, err := p.Post(ctx, "comm", "conversion", "", nowUTC(), []ledger.Line{
		{Account: "EXPENSE:COMMISSION:USD", Amount: 300},
		{Account: "AFFILIATE_PAYABLE:1:USD", Amount: -300},
	})
	if err != nil {
		t.Fatal(err)
	}
	var cid int64
	if err := h.DB.QueryRow(`INSERT INTO commissions (affiliate_id, journal_id, amount, rate_snapshot, status)
		VALUES (1, ?, 300, '{}', 'pending') RETURNING id`, jid).Scan(&cid); err != nil {
		t.Fatal(err)
	}

	// pending -> approved: legal
	if _, err := h.DB.Exec(`UPDATE commissions SET status='approved', approved_at=? WHERE id=?`, nowUTC(), cid); err != nil {
		t.Fatalf("pending->approved rejected: %v", err)
	}
	// approved -> paid: legal
	if _, err := h.DB.Exec(`UPDATE commissions SET status='paid', paid_at=? WHERE id=?`, nowUTC(), cid); err != nil {
		t.Fatalf("approved->paid rejected: %v", err)
	}
	// paid -> pending: illegal
	if _, err := h.DB.Exec(`UPDATE commissions SET status='pending' WHERE id=?`, cid); err == nil {
		t.Fatal("paid->pending accepted")
	}
	// amount mutation: illegal
	if _, err := h.DB.Exec(`UPDATE commissions SET amount=999 WHERE id=?`, cid); err == nil {
		t.Fatal("amount mutation accepted")
	}
}

// P11: payout money immutability.
func TestProperty_11_PayoutImmutable(t *testing.T) {
	h, _ := newTestDB(t)
	var pid int64
	if err := h.DB.QueryRow(`INSERT INTO payouts (affiliate_id, amount) VALUES (1, 100) RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE payouts SET amount=999 WHERE id=?`, pid); err == nil {
		t.Fatal("payout amount mutation accepted")
	}
	// status transitions are allowed (not money fields)
	if _, err := h.DB.Exec(`UPDATE payouts SET status='approved' WHERE id=?`, pid); err != nil {
		t.Fatalf("payout status transition rejected: %v", err)
	}
}

// P12: raw_events uniqueness — the ingest idempotency axis (EXP-11 lesson).
func TestProperty_12_RawEventUnique(t *testing.T) {
	h, _ := newTestDB(t)
	if _, err := h.DB.Exec(`INSERT INTO raw_events (source, external_event_id, event_type, payload)
		VALUES ('stripe', 'evt_1', 'payment', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO raw_events (source, external_event_id, event_type, payload)
		VALUES ('stripe', 'evt_1', 'payment', '{}')`); err == nil {
		t.Fatal("duplicate raw_event accepted — double-money path open")
	}
}

// P13: outbox undelivered partial index works.
func TestProperty_13_OutboxIndex(t *testing.T) {
	h, _ := newTestDB(t)
	if _, err := h.DB.Exec(`INSERT INTO outbox_events (topic, payload) VALUES ('commission.created', '{}')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE delivered_at IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("undelivered=%d err=%v, want 1", n, err)
	}
}

// P14: webhook_events uniqueness (second replay guard, signature-gated tier).
func TestProperty_14_WebhookEventUnique(t *testing.T) {
	h, _ := newTestDB(t)
	// TRUE is a boolean literal on PG and evaluates to 1 on SQLite; the
	// integer 1 is fine on SQLite but SQLSTATE 42804 on PG.
	if _, err := h.DB.Exec(`INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
		VALUES ('stripe', 'evt_w1', TRUE, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO webhook_events (source, external_event_id, signature_ok, payload)
		VALUES ('stripe', 'evt_w1', TRUE, '{}')`); err == nil {
		t.Fatal("duplicate webhook_event accepted")
	}
}

// P15: GlobalIntegrity helper — sum of all entries is always 0.
func TestProperty_15_IntegrityHelper(t *testing.T) {
	_, p := newTestDB(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := p.Post(ctx, fmt.Sprintf("integ-%d", i), "conversion", "", nowUTC(), lines(int64(50*i+1))); err != nil {
			t.Fatal(err)
		}
	}
	s, err := p.GlobalIntegrity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s != 0 {
		t.Fatalf("integrity=%d, want 0", s)
	}
}

// P16: the shared Poster on the ONE dedicated ledger conn is safe for
// concurrent use. Production shape: commission engine, payout runner and
// fraud sweep all drive the same Poster. *sql.Conn is not concurrency-safe
// (database/sql docs); without serialization, racing callers interleave the
// wire protocol on the pinned session and surface as `driver: bad
// connection` / `sql: expected 0 destination arguments in Scan` mid-post
// (observed session 16 on PG). Contract: no error, every journal commits
// exactly once, global sum stays 0.
func TestProperty_16_SharedPosterConcurrency(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()

	const workers = 8
	const per = 25

	var posters sync.WaitGroup
	errCh := make(chan error, workers+2)
	for w := 0; w < workers; w++ {
		posters.Add(1)
		go func(w int) {
			defer posters.Done()
			for i := 0; i < per; i++ {
				idem := fmt.Sprintf("race-%d-%d", w, i)
				if _, err := p.Post(ctx, idem, "conversion", "", nowUTC(), lines(int64(100+w))); err != nil {
					errCh <- fmt.Errorf("post %s: %w", idem, err)
					return
				}
				if _, err := p.AccountBalance(ctx, "HOLD:USD"); err != nil {
					errCh <- fmt.Errorf("balance after %s: %w", idem, err)
					return
				}
			}
		}(w)
	}

	// Reader goroutine hammering the same conn while posts land.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := p.GlobalIntegrity(ctx); err != nil {
				errCh <- fmt.Errorf("integrity read: %w", err)
				return
			}
			if _, err := p.AccountBalance(ctx, "EXPENSE:COMMISSION:USD"); err != nil {
				errCh <- fmt.Errorf("balance read: %w", err)
				return
			}
		}
	}()

	posters.Wait()
	close(stop)
	readers.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if n := journalCount(t, h.DB); n != workers*per {
		t.Fatalf("journals=%d, want %d", n, workers*per)
	}
	// HOLD receives -amount per post: -(per * sum(100+w for w in 0..7)).
	var wantHold int64
	for w := 0; w < workers; w++ {
		wantHold -= int64(per) * int64(100+w)
	}
	bal, err := p.AccountBalance(ctx, "HOLD:USD")
	if err != nil {
		t.Fatal(err)
	}
	if bal != wantHold {
		t.Fatalf("HOLD balance=%d, want %d", bal, wantHold)
	}
	if s, err := p.GlobalIntegrity(ctx); err != nil || s != 0 {
		t.Fatalf("integrity=%d err=%v, want 0 nil", s, err)
	}
}
