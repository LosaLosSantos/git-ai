package resolve

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
)

// nanoUSD is an amount of money in billionths of a US dollar. The budget counts in integers, so
// that sums are exact and a comparison with the cap needs no tolerance: a float64 sum of 49 calls
// of $0.01 is a little more than $0.49, and 50 such calls would pass a $0.50 cap.
type nanoUSD int64

const nanoPerUSD = 1_000_000_000

// maxNanoUSD bounds every amount (about $9.2 billion), far above any cap or call.
const maxNanoUSD = nanoUSD(math.MaxInt64)

// usdToNano converts a configured amount in USD exactly, from the shortest decimal that
// represents the float64 (the number as written in the configuration), rounding a fraction of a
// nano-dollar up.
func usdToNano(usd float64) (nanoUSD, error) {
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("invalid amount %v", usd)
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(usd, 'g', -1, 64))
	if !ok {
		return 0, fmt.Errorf("invalid amount %v", usd)
	}
	r.Mul(r, big.NewRat(nanoPerUSD, 1))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if new(big.Rat).SetInt(n).Cmp(r) < 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("amount %v is too large", usd)
	}
	return nanoUSD(n.Int64()), nil
}

// USD returns the amount in dollars, for display.
func (n nanoUSD) USD() float64 { return float64(n) / nanoPerUSD }

// String writes the amount as an exact decimal number of dollars, "1.250000000".
func (n nanoUSD) String() string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%09d", sign, n/nanoPerUSD, n%nanoPerUSD)
}

// parseNanoUSD reads an exact decimal number of dollars, as String writes it.
func parseNanoUSD(s string) (nanoUSD, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	r.Mul(r, big.NewRat(nanoPerUSD, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, fmt.Errorf("amount %q is not a whole number of nano-dollars", s)
	}
	return nanoUSD(r.Num().Int64()), nil
}

// tokenCost returns tokens × perMillion / 1e6, rounded up, without overflow: the cost of tokens
// at a price per million tokens.
func tokenCost(tokens int, perMillion nanoUSD) (nanoUSD, error) {
	if tokens < 0 {
		return 0, errors.New("negative token count")
	}
	hi, lo := bits.Mul64(uint64(tokens), uint64(perMillion))
	if hi >= 1_000_000 {
		return 0, errors.New("cost overflows")
	}
	q, rem := bits.Div64(hi, lo, 1_000_000)
	if rem != 0 {
		q++
	}
	if q > uint64(maxNanoUSD) {
		return 0, errors.New("cost overflows")
	}
	return nanoUSD(q), nil
}

// addNano adds amounts, failing on overflow.
func addNano(a ...nanoUSD) (nanoUSD, error) {
	var sum nanoUSD
	for _, v := range a {
		if v > maxNanoUSD-sum {
			return 0, errors.New("amount overflows")
		}
		sum += v
	}
	return sum, nil
}
