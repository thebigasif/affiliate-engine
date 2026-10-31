// Package moneylock provides in-process, per-entity mutexes for the
// money-mutating paths (maturation, clawback, fraud uphold, payout settle).
//
// These locks are an in-process optimisation, not the correctness
// mechanism. Each money path commits its journal and domain rows in one
// ledger transaction (ledger.Poster.Run) that first locks the rows it
// decides on (SELECT ... FOR UPDATE on Postgres; SQLite's BEGIN IMMEDIATE
// holds the database write lock), so two replicas on one Postgres stay
// serialized without them. Within a process they queue same-entity rivals
// here, cheaply, instead of on the ledger conn mutex — and payouts.Approve
// still relies on them to hold a payout's commissions across its rail
// call, which no database transaction spans.
//
// Lock discipline:
//   - acquire ONE commission lock, or a SET of them through LockAll
//     (payouts.Approve); never hand-roll a multi-lock acquisition.
//   - never take a second moneylock while holding one, except the set
//     acquisition in Approve.
//   - locks are held across the ledger post ON PURPOSE: that is the window
//     under attack.
package moneylock

import (
	"context"
	"hash/fnv"
	"slices"
	"sync"
)

// STRIPES: bounded lock pool (audit P1-4 — the per-name map grew ~100B per
// commission forever; 1M commissions = ~100MB in the long-running daemon).
// Striping gives identical per-entity serialization (same name -> same
// stripe deterministically) with O(1) fixed memory. Trade-off: unrelated
// entities sharing a stripe serialize — an extra mutex wait, never a
// correctness effect (the lock contract is mutual exclusion on the name,
// which striping preserves).
const stripes = 4096

var pool [stripes]sync.Mutex

func stripeIndex(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32() % stripes
}

// Lock acquires the mutex for the named entity, blocking until available.
// It returns the unlock function; callers must defer it immediately.
func Lock(name string) func() {
	m := &pool[stripeIndex(name)]
	m.Lock()
	return m.Unlock
}

// heldKey marks a ctx whose caller already holds every money lock the
// work under it needs.
type heldKey struct{}

// Held marks ctx: the caller took, through LockAll and BEFORE it opened
// the transaction the work under ctx joins, the money lock of every
// existing entity that work touches, and holds them until that
// transaction has committed or rolled back. LockIn under a held ctx is a
// no-op.
//
// Why: a lock taken inside a JOINED ledger transaction (one an outer
// caller opened, ledger.PooledPoster.Run nesting) would be released when
// the inner call returns, while its writes are still uncommitted; a
// rival (payouts.Approve) would then act on state that is about to
// change. And a second lock taken while holding a set breaks the
// lock order (an AB-BA deadlock Postgres cannot see). So the outer
// holder takes the whole set up front, in LockAll's order, and the
// joined calls take nothing.
func Held(ctx context.Context) context.Context { return context.WithValue(ctx, heldKey{}, true) }

// IsHeld reports a ctx marked by Held.
func IsHeld(ctx context.Context) bool {
	v, _ := ctx.Value(heldKey{}).(bool)
	return v
}

// LockIn is Lock, except under a ctx marked by Held, where the caller
// already holds the lock set and it returns a no-op unlock. Under a ctx
// carrying a scope (WithScope) the name is the scoped one.
func LockIn(ctx context.Context, name string) func() {
	if IsHeld(ctx) {
		return func() {}
	}
	return Lock(Scoped(ctx, name))
}

// scopeKey carries the tenant scope of money locks.
type scopeKey struct{}

// WithScope marks ctx with a tenant scope (the Shopify shop): every
// lock taken through LockIn or LockAllIn under it is named
// "<scope>|<name>", so the same entity id in two shops never shares a
// lock and one shop's blocked money work cannot hold up another's
// (PRD 12.4 AC2, OPEN-W3-4). The scope must be set for every path that
// locks a given shop's entities, or two paths would stop excluding each
// other: callers that hold a lock set use LockAllIn with the same ctx.
func WithScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// Scoped returns name under ctx's scope ("<scope>|<name>"), or name
// itself when ctx has none.
func Scoped(ctx context.Context, name string) string {
	if s, _ := ctx.Value(scopeKey{}).(string); s != "" {
		return s + "|" + name
	}
	return name
}

// LockAllIn is LockAll with every name under ctx's scope.
func LockAllIn(ctx context.Context, names []string) func() {
	scoped := make([]string, len(names))
	for i, n := range names {
		scoped[i] = Scoped(ctx, n)
	}
	return LockAll(scoped)
}

// Commission returns the lock name for a commission row.
func Commission(id int64) string { return "commission:" + int64str(id) }

// Payout returns the lock name for a payout row.
func Payout(id int64) string { return "payout:" + int64str(id) }

// LockAll acquires the mutexes for every named entity and returns the
// single unlock func.
//
// The unit of locking is the STRIPE, so the set is reduced to its distinct
// stripes and those are locked in ascending stripe order. Sorting by NAME
// is not enough: two names that share a stripe need not be adjacent in
// name order (the stripe would be locked twice — a self-deadlock), and two
// callers could take the same pair of stripes in opposite orders (an AB-BA
// deadlock) even though their name lists sort identically.
func LockAll(names []string) func() {
	idx := make([]uint32, 0, len(names))
	for _, n := range names {
		idx = append(idx, stripeIndex(n))
	}
	slices.Sort(idx)
	idx = slices.Compact(idx)
	for _, i := range idx {
		pool[i].Lock()
	}
	return func() {
		for k := len(idx) - 1; k >= 0; k-- {
			pool[idx[k]].Unlock()
		}
	}
}

func int64str(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [21]byte
	i := len(buf)
	u := uint64(v) // #nosec G115 -- intentional two's-complement reinterpretation; if v<0 the magnitude is recomputed below
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
