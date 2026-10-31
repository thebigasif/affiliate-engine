package moneylock_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebigasif/affiliate-engine/internal/moneylock"
)

// The moneylock package serializes the CAS->journal-post->revert window on
// every money-mutating path (uphold, Mature, ProcessRefund, payout
// Approve). These tests pin its contract; they are only meaningful under
// `-race`, which every gate run uses.

// M1: Lock(name) is a true mutex — the critical section is never entered
// by two goroutines. The guard variable is a PLAIN bool: without the lock
// the race detector fires immediately; with it, silence is proof.
func TestMoneyLock_MutualExclusion(t *testing.T) {
	var inSection bool
	var violations atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			unlock := moneylock.Lock("commission:1")
			if inSection {
				violations.Add(1)
			}
			inSection = true
			// Simulate the CAS->post->revert window: long enough for the
			// scheduler to interleave a rival goroutine if the lock leaks.
			time.Sleep(time.Millisecond)
			inSection = false
			unlock()
		}(i)
	}
	wg.Wait()
	if v := violations.Load(); v != 0 {
		t.Fatalf("%d overlapping critical sections — Lock is not excluding", v)
	}
}

// M2: different names are independent locks — holding one must not block
// another (a per-commission lock that serialized the whole process would
// kill payout throughput and hide real bugs).
func TestMoneyLock_DistinctNamesIndependent(t *testing.T) {
	unlockA := moneylock.Lock("commission:100")
	acquired := make(chan struct{})
	go func() {
		unlockB := moneylock.Lock("commission:200")
		close(acquired)
		unlockB()
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("Lock(commission:200) blocked while commission:100 held — distinct names must be independent")
	}
	unlockA()
}

// M3: same name across Lock and LockAll — LockAll must contend with plain
// Lock on an overlapping name (payouts.Approve holds the sorted set while
// uphold holds the single commission lock).
func TestMoneyLock_LockAllContendsWithLock(t *testing.T) {
	unlock := moneylock.Lock("commission:7")
	acquired := make(chan struct{})
	go func() {
		unAll := moneylock.LockAll([]string{"commission:6", "commission:7", "commission:8"})
		close(acquired)
		unAll()
	}()
	select {
	case <-acquired:
		t.Fatal("LockAll acquired commission:7 while plain Lock held it")
	case <-time.After(150 * time.Millisecond):
	}
	unlock() // now LockAll must complete promptly.
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("LockAll did not acquire after the blocking Lock was released")
	}
}

// M4: LockAll acquires its set in SORTED order — two goroutines passing
// the SAME names in OPPOSITE orders must both complete instead of
// deadlocking (the classic AB-BA shape; unsorted acquisition would hang).
func TestMoneyLock_LockAllOrderDeadlockSafe(t *testing.T) {
	names := []string{"payout:5", "commission:3", "commission:1", "commission:2", "commission:4"}
	reversed := make([]string, len(names))
	for i, n := range names {
		reversed[len(names)-1-i] = n
	}

	done := make(chan struct{}, 2)
	for _, set := range [][]string{names, reversed, shuffledRotate(names), shuffledRotate(reversed)} {
		set := set
		go func() {
			unlock := moneylock.LockAll(set)
			time.Sleep(2 * time.Millisecond) // hold the full set briefly
			unlock()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("LockAll deadlocked on permuted input orders — acquisition is not sorted")
		}
	}
}

func shuffledRotate(in []string) []string {
	out := make([]string, len(in))
	copy(out, in[1:])
	out[len(out)-1] = in[0]
	return out
}

// M5: duplicate names in one LockAll call are collapsed — the unlock
// function must release exactly once per unique name (double-Unlock on a
// sync.Mutex panics, so a naive expansion would crash here).
func TestMoneyLock_LockAllDuplicatesCollapsed(t *testing.T) {
	const name = "payout:9"
	unlock := moneylock.LockAll([]string{name, name, name, name})
	// If duplicates were expanded, this second Lock would either block
	// forever (self-deadlock) or the unlock below would panic.
	acquired := make(chan struct{})
	go func() {
		u2 := moneylock.Lock(name)
		close(acquired)
		u2()
	}()
	select {
	case <-acquired:
		t.Fatal("Lock(payout:9) acquired while LockAll([payout:9 x4]) held it")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("Lock(payout:9) did not acquire after LockAll unlocked — duplicate names broke release")
	}
}

// M6: the production shape — many goroutines each grab a small SORTED set
// of commission locks (as payouts.Approve does) over a crowded keyspace,
// simulating the read->post->revert window. The race detector watches the
// plain (non-atomic) per-commission flags; any overlap is a data race.
func TestMoneyLock_StressApproveShape(t *testing.T) {
	const keyset = 24 // distinct commissions the rivals fight over
	inUse := make([]bool, keyset)
	var wg sync.WaitGroup
	for w := 0; w < 48; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 5; round++ {
				// Each worker takes a contiguous slice of 3 commissions —
				// overlapping windows across workers, exactly like
				// concurrent payout batches over shared affiliates.
				base := (worker*7 + round*5) % keyset
				var names []string
				for k := 0; k < 3; k++ {
					names = append(names, moneylock.Commission(int64((base+k)%keyset+1)))
				}
				unlock := moneylock.LockAll(names)
				for k := 0; k < 3; k++ {
					idx := (base + k) % keyset
					if inUse[idx] {
						t.Errorf("commission %d entered by two holders", idx+1)
					}
					inUse[idx] = true
				}
				time.Sleep(500 * time.Microsecond) // the post window
				for k := 0; k < 3; k++ {
					inUse[(base+k)%keyset] = false
				}
				unlock()
			}
		}(w)
	}
	wg.Wait()
}

// M7: name derivation — stable, collision-free across kinds, well-defined
// for zero and negative ids (import/rollback paths use synthesized ids).
func TestMoneyLock_Names(t *testing.T) {
	if got := moneylock.Commission(42); got != "commission:42" {
		t.Errorf("Commission(42) = %q", got)
	}
	if got := moneylock.Payout(0); got != "payout:0" {
		t.Errorf("Payout(0) = %q", got)
	}
	if got := moneylock.Commission(-3); got != "commission:-3" {
		t.Errorf("Commission(-3) = %q", got)
	}
	if moneylock.Commission(1) == moneylock.Payout(1) {
		t.Errorf("commission and payout namespaces collide")
	}
	// Lexicographic vs numeric ordering diverges (commission:10 < commission:9
	// as strings). LockAll sorts name STRINGS — that is safe (deadlock
	// freedom needs a total order, not a numeric one) but callers must not
	// assume numeric ordering. Pin the actual behavior so a future change
	// to the naming scheme re-examines LockAll.
	a := moneylock.Commission(9)
	b := moneylock.Commission(10)
	if !(a > b) {
		t.Errorf("expected lexicographic sort to differ from numeric: %q vs %q", a, b)
	}
	_ = fmt.Sprint(a, b)
}
