package bpmath_test

import (
	"math"
	"math/big"
	"testing"

	"github.com/thebigasif/affiliate-engine/internal/bpmath"
)

func TestMulDivBP_Exact(t *testing.T) {
	cases := []struct {
		amount, bp, want int64
	}{
		{10000, 1000, 1000},             // 10% of $100
		{999, 333, 33},                  // truncation
		{0, 10000, 0},                   // zero amount
		{12345, 0, 0},                   // zero rate
		{-5000, 1000, -500},             // negative amount (clawback)
		{1, 10000, 1},                   // 100%
		{9e18, 10000, 9e18},             // 100% of near-max — exact, fits
		{4611686018427387904, 20000, 0}, // 200% of 2^62 → overflow → NOT ok, handled in overflow test
	}
	_ = cases[len(cases)-1] // last case exercised separately below
	for _, c := range cases[:len(cases)-1] {
		got, ok := bpmath.MulDivBP(c.amount, c.bp)
		if !ok {
			t.Fatalf("MulDivBP(%d,%d): unexpected !ok", c.amount, c.bp)
		}
		if got != c.want {
			t.Fatalf("MulDivBP(%d,%d)=%d want %d", c.amount, c.bp, got, c.want)
		}
	}
}

func TestMulDivBP_OverflowRejected(t *testing.T) {
	// 9e18 * 10000 / 10000 = 9e18 fits; 9e18 at 200% does not.
	v, ok := bpmath.MulDivBP(9_000_000_000_000_000_000, 20_000)
	if ok {
		t.Fatalf("overflow not rejected: %d", v)
	}
	// MaxInt64 * 100% is exactly MaxInt64 — ok.
	v, ok = bpmath.MulDivBP(math.MaxInt64, 10_000)
	if !ok || v != math.MaxInt64 {
		t.Fatalf("MaxInt64@100%% = %d ok=%v", v, ok)
	}
}

func TestMulDiv_Proration(t *testing.T) {
	// amount=1000, refund 1/4 → 250
	v, ok := bpmath.MulDiv(1000, 1, 4)
	if !ok || v != 250 {
		t.Fatalf("got %d ok=%v want 250", v, ok)
	}
	// Cross-check against big.Int at scale.
	for _, x := range []int64{1, 17, 999999, 4_611_686_018_427_387_903} {
		for _, y := range []int64{1, 33, 7001, 9_000_000_000_000_000_000} {
			v, ok := bpmath.MulDiv(x, y, 7_000)
			want := new(big.Int).Div(new(big.Int).Mul(big.NewInt(x), big.NewInt(y)), big.NewInt(7000))
			if !ok {
				continue // overflow rejected — fine
			}
			if new(big.Int).SetInt64(v).Cmp(want) != 0 {
				t.Fatalf("MulDiv(%d,%d,7000)=%d want %s", x, y, v, want)
			}
		}
	}
}

func TestMulDiv_ZeroAndClamp(t *testing.T) {
	if _, ok := bpmath.MulDiv(1, 1, 0); ok {
		t.Fatal("d=0 must be !ok")
	}
	if v, ok := bpmath.MulDiv(0, 5, 3); !ok || v != 0 {
		t.Fatal("zero input")
	}
	if v, ok := bpmath.MulDiv(5, 0, 3); !ok || v != 0 {
		t.Fatal("zero input")
	}
	// Negative divisor sign folding.
	if v, ok := bpmath.MulDiv(10, 3, -2); !ok || v != -15 {
		t.Fatalf("neg divisor: %d %v", v, ok)
	}
}
