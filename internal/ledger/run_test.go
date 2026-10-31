package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/ledger"
)

// RUN-1: a domain write and a journal post in one Run commit together,
// and a failure inside fn rolls BOTH back.
func TestRun_AtomicDomainAndJournal(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()

	if err := p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO affiliates (id, email) VALUES (?, ?)`, 7, "seven@test.dev"); err != nil {
			return err
		}
		_, err := tx.Post(ctx, "run-ok", "conversion", "", nowUTC(), lines(500))
		return err
	}); err != nil {
		t.Fatalf("commit path: %v", err)
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM affiliates WHERE id = 7`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("domain row missing after commit (n=%d err=%v)", n, err)
	}
	if c := journalCount(t, h.DB); c != 1 {
		t.Fatalf("journals=%d want 1", c)
	}

	// The journal posts (through Poster.Post with fn's ctx: it joins),
	// then a domain write fails: neither may persist.
	err := p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		if _, err := p.Post(ctx, "run-rb", "conversion", "", nowUTC(), lines(900)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO affiliates (id, email) VALUES (?, ?)`, 7, "dupe@test.dev")
		return err
	})
	if err == nil {
		t.Fatal("rollback path committed")
	}
	if c := journalCount(t, h.DB); c != 1 {
		t.Fatalf("journal from a rolled-back Run persisted: %d journals, want 1", c)
	}
	if s := entrySum(t, h.DB); s != 0 {
		t.Fatalf("entry sum=%d", s)
	}
}

// RUN-2: ErrDuplicate inside a Run surfaces with the existing journal id
// and leaves the transaction usable (the replay-converge shape).
func TestRun_DuplicateInsideRun(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	first, err := p.Post(ctx, "run-dup", "conversion", "", nowUTC(), lines(1000))
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		var err error
		got, err = tx.Post(ctx, "run-dup", "conversion", "", nowUTC(), lines(1000))
		if !errors.Is(err, ledger.ErrDuplicate) {
			return fmt.Errorf("want ErrDuplicate, got %v", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO affiliates (id, email) VALUES (8, 'e@test.dev')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("duplicate returned id %d, want %d", got, first)
	}
	if c := journalCount(t, h.DB); c != 1 {
		t.Fatalf("journals=%d want 1", c)
	}
}

// RUN-3: a nested Run (and AccountBalance) with fn's ctx joins the open
// transaction instead of deadlocking on the conn mutex, and reads its
// uncommitted writes. A second Poster on the same conn joins too.
func TestRun_NestedJoins(t *testing.T) {
	h, p := newTestDB(t)
	ctx := context.Background()
	other := ledger.NewPoster(h.Ledger)
	if err := p.Run(ctx, func(ctx context.Context, tx *ledger.Tx) error {
		if _, err := tx.Post(ctx, "nest-1", "conversion", "", nowUTC(), lines(300)); err != nil {
			return err
		}
		if err := other.Run(ctx, func(ctx context.Context, _ *ledger.Tx) error {
			_, err := other.Post(ctx, "nest-2", "conversion", "", nowUTC(), lines(200))
			return err
		}); err != nil {
			return err
		}
		bal, err := p.AccountBalance(ctx, "HOLD:USD")
		if err != nil {
			return err
		}
		if bal != -500 {
			return fmt.Errorf("in-tx HOLD=%d want -500", bal)
		}
		return errors.New("roll back")
	}); err == nil || err.Error() != "roll back" {
		t.Fatalf("run: %v", err)
	}
	if c := journalCount(t, h.DB); c != 0 {
		t.Fatalf("journals=%d after rollback, want 0 (nested posts are part of the outer tx)", c)
	}
}
