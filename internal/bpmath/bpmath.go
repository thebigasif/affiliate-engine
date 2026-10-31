// Package bpmath computes money ratios without int64 overflow or floats:
// amount*rateBP/10000 and x*y/d, exact via 128-bit intermediates. The
// attack suite proved four independent wrap/float sites (commission percent
// math, usage settlement basis, MLM tier split promoting to REAL on SQLite,
// clawback proration via float64). Every money multiply goes through here.
package bpmath

import (
	"math"
	"math/big"
	"math/bits"
)

const scale = 10000

// MulDivBP returns amount*rateBP/10000 exactly (truncating division, as
// integer money math always has). ok=false means the exact quotient does
// not fit in int64 — callers must reject (never clamp/wrap).
func MulDivBP(amount, rateBP int64) (v int64, ok bool) {
	return MulDiv(amount, rateBP, scale)
}

// MulDiv returns x*y/d exactly (truncating division). ok=false means the
// quotient does not fit in int64, or d==0.
func MulDiv(x, y, d int64) (v int64, ok bool) {
	if d == 0 {
		return 0, false
	}
	if x == 0 || y == 0 {
		return 0, true
	}
	neg := (x < 0) != (y < 0)
	if d < 0 {
		d = -d
		neg = !neg
	}
	hi, lo := bits.Mul64(uabs(x), uabs(y))
	var q *big.Int
	if hi == 0 {
		q = new(big.Int).SetUint64(lo / uint64(d)) // #nosec G115 -- d>0 (negated above); lo/uint64(d) <= lo < 2^64, checked against MaxInt64 below
	} else {
		// 128-bit product: exact path via big.Int (per-conversion math,
		// not hot-path; bits.Div64 panics when hi >= d).
		prod := new(big.Int).Lsh(big.NewInt(int64(hi)), 64) // #nosec G115 -- hi < 2^64 by construction of bits.Mul64's high word, fits int64 after the 128-bit bound check below
		prod.Add(prod, new(big.Int).SetUint64(lo))
		q = new(big.Int).Quo(prod, new(big.Int).SetUint64(uint64(d))) // #nosec G115 -- d>0 here (negated above)
	}
	if !q.IsUint64() || q.Uint64() > uint64(math.MaxInt64) {
		return 0, false
	}
	v = int64(q.Uint64()) // #nosec G115 -- q.Uint64() <= MaxInt64 checked on the line above
	if neg {
		v = -v
	}
	return v, true
}

// uabs returns the magnitude of x as uint64. For math.MinInt64 the negation
// wraps to itself; uint64(x) is then already the correct 2^63 magnitude.
func uabs(x int64) uint64 {
	if x >= 0 {
		return uint64(x)
	}
	return uint64(-x)
}
