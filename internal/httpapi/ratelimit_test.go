package httpapi

import (
	"fmt"
	"testing"
	"time"
)

// P1-4 pin: the rate limiter's maps stay bounded under adversarial key
// churn (IPv6 rotation). Sweep evicts cold entries; live buckets survive.
func TestRateLimiter_BoundedUnderKeyChurn(t *testing.T) {
	rl := newRateLimiter(10)
	// Simulate 100k distinct keys (rotator attack).
	for i := 0; i < 100_000; i++ {
		rl.allow(fmt.Sprintf("pub:2001:db8::%x", i))
	}
	// Force the lazy sweep window by backdating everything but one key.
	now := time.Now().Unix() / 60
	rl.mu.Lock()
	for k := range rl.seen {
		rl.seen[k] = now - 5 // all cold
	}
	rl.mu.Unlock()
	// One more call trips the sweep (calls&4095==0 after 100000+... make it deterministic):
	rl.mu.Lock()
	rl.calls = 4095
	rl.mu.Unlock()
	rl.allow("pub:keepme")
	rl.mu.Lock()
	n := len(rl.seen)
	rl.mu.Unlock()
	if n > 2 { // keepme + possibly the swept entry
		t.Fatalf("limiter maps unbounded: %d entries after sweep (want <=2)", n)
	}
	// The live bucket still rate-limits correctly. Minute-boundary-safe:
	// verify via the maps directly (a real minute roll between allow()
	// calls would legitimately recycle the count).
	rl.mu.Lock()
	if rl.counts["pub:keepme"] == 0 {
		rl.mu.Unlock()
		t.Fatal("live bucket evicted by sweep")
	}
	rl.counts["pub:keepme"] = 10 // at the limit
	rl.mu.Unlock()
	if rl.allow("pub:keepme") {
		t.Fatal("limit not enforced after sweep")
	}
}
