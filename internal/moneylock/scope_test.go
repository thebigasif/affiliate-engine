package moneylock

import (
	"context"
	"testing"
	"time"
)

// blocks reports whether lock waits on a lock the caller holds.
func blocks(lock func() func()) bool {
	done := make(chan func(), 1)
	go func() { done <- lock() }()
	select {
	case u := <-done:
		u()
		return false
	case <-time.After(100 * time.Millisecond):
		// Leave the goroutine to finish once the holder releases.
		go func() { (<-done)() }()
		return true
	}
}

// TestScopedLocks (OPEN-W3-4): the same entity in two scopes never
// shares a lock, the same entity in one scope still excludes across
// LockIn and LockAllIn, and an unscoped ctx keeps the old names.
func TestScopedLocks(t *testing.T) {
	a := WithScope(context.Background(), "shop_aaaa")
	b := WithScope(context.Background(), "shop_bbbb")
	name := Commission(1)
	if Scoped(a, name) == Scoped(b, name) || Scoped(context.Background(), name) != name {
		t.Fatal("scoped names")
	}

	unlock := LockIn(a, name)
	// Another shop's commission 1 is free (unless the two scoped names
	// happen to share a stripe, which these do not).
	if stripeIndex(Scoped(a, name)) != stripeIndex(Scoped(b, name)) &&
		blocks(func() func() { return LockIn(b, name) }) {
		t.Fatal("shop B blocked on shop A's commission 1")
	}
	// Same shop: a set lock waits for the single lock.
	if !blocks(func() func() { return LockAllIn(a, []string{name, Commission(2)}) }) {
		t.Fatal("same-shop set lock did not wait")
	}
	unlock()

	// Held still short-circuits under a scope.
	held := Held(a)
	u := LockIn(held, name)
	u()
}
