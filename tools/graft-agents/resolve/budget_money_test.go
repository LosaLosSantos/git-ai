package resolve

import (
	"encoding/json"
	"math"
	"testing"
)

func TestUSDToNanoIsExact(t *testing.T) {
	for usd, want := range map[float64]nanoUSD{
		0:                   0,
		0.2:                 200_000_000, // 0.2 is not exact in binary; its shortest decimal is
		0.30000000000000004: 300_000_001, // the float sum 0.1+0.2: the fraction of a nano-dollar rounds up
		10:                  10_000_000_000,
		0.0000000001:        1, // a tenth of a nano-dollar rounds up to one
		1e-9:                1,
	} {
		got, err := usdToNano(usd)
		if err != nil || got != want {
			t.Errorf("usdToNano(%v) = %d, %v; want %d", usd, got, err, want)
		}
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1), 1e12} {
		if _, err := usdToNano(bad); err == nil {
			t.Errorf("usdToNano(%v) accepted", bad)
		}
	}
}

func TestTokenCostRoundsUpWithoutOverflow(t *testing.T) {
	for _, tc := range []struct {
		tokens     int
		perMillion nanoUSD
		want       nanoUSD
	}{
		{1_000_000, 200_000_000, 200_000_000},
		{1, 200_000_000, 200},
		{3, 1, 1}, // 3e-6 of a nano-dollar is booked as one
		{0, 200_000_000, 0},
		{1 << 30, 1_000_000_000_000, 1_073_741_824_000_000}, // a billion tokens at $1000 per million
	} {
		got, err := tokenCost(tc.tokens, tc.perMillion)
		if err != nil || got != tc.want {
			t.Errorf("tokenCost(%d, %d) = %d, %v; want %d", tc.tokens, tc.perMillion, got, err, tc.want)
		}
	}
	if _, err := tokenCost(math.MaxInt64, maxNanoUSD); err == nil {
		t.Error("overflow not detected")
	}
	if _, err := tokenCost(-1, 1); err == nil {
		t.Error("negative tokens accepted")
	}
	if _, err := addNano(maxNanoUSD, 1); err == nil {
		t.Error("sum overflow not detected")
	}
}

func TestBudgetStateJSONIsExact(t *testing.T) {
	st := budgetState{Month: "2026-10", Spent: 1_250_000_001}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"month":"2026-10","spent_usd":1.250000001}` {
		t.Fatalf("state %s", data)
	}
	var back budgetState
	if err := json.Unmarshal(data, &back); err != nil || back != st {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
	// A hand-written state file with a short decimal is read exactly.
	if err := json.Unmarshal([]byte(`{"month":"2026-10","spent_usd":4.9999}`), &back); err != nil || back.Spent != 4_999_900_000 {
		t.Fatalf("short decimal: %+v, %v", back, err)
	}
	for _, bad := range []string{`{"month":"2026-10","spent_usd":-1}`, `{"month":"2026-10","spent_usd":0.0000000001}`, `{"month":"2026-10","spent_usd":"x"}`} {
		if err := json.Unmarshal([]byte(bad), &back); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestBudgetCapIsExactAtTheBoundary is the case that float sums got wrong: fifty calls of $0.01
// fit a $0.50 cap exactly, and the fifty-first does not.
func TestBudgetCapIsExactAtTheBoundary(t *testing.T) {
	b, err := NewBudget(0.5, Prices{InputPerMillion: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := b.Reserve(10_000, 0); err != nil { // $0.01
			t.Fatalf("call %d refused: %v", i+1, err)
		}
	}
	if _, err := b.Reserve(1, 0); err == nil {
		t.Fatal("a call past the cap was allowed")
	}
}
