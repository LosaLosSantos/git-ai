package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// Prices are the prices of a model in USD. OutputPerMillion applies to every output token,
// reasoning included (providers count reasoning tokens as output).
type Prices struct {
	InputPerMillion  float64
	OutputPerMillion float64
	// PerRequest is a fixed price per call, when the provider charges one.
	PerRequest float64
}

func (p Prices) cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.InputPerMillion/1e6 + float64(outputTokens)*p.OutputPerMillion/1e6 + p.PerRequest
}

func (p Prices) validate() error {
	for _, v := range []float64{p.InputPerMillion, p.OutputPerMillion, p.PerRequest} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("invalid price %v", v)
		}
	}
	if p.InputPerMillion == 0 && p.OutputPerMillion == 0 && p.PerRequest == 0 {
		return errors.New("no price is set")
	}
	return nil
}

// Budget tracks the AI spend of a calendar month (UTC) and caps it (PLAN10 3.5). Before every
// model call the worst case of the call is reserved: the spend is raised by it, and the call is
// not made when that would exceed the cap. After the call the reservation is replaced by the
// cost of the usage the provider reports. So the recorded spend is never below the real one, and
// a process that dies between the two steps leaves its reservation booked.
//
// With a state file the spend survives restarts and is shared by every graft-resolve process
// that uses the file: each update holds an exclusive lock (flock on state + ".lock"), reads the
// file, and writes it again atomically (temporary file, fsync, rename, fsync of the directory).
// Without a state file the spend lives in memory (the benchmark).
type Budget struct {
	// MonthlyUSD is the cap; 0 means no cap (the spend is still tracked).
	MonthlyUSD float64
	Prices     Prices

	state string
	now   func() time.Time

	mu    sync.Mutex
	mem   budgetState
	calls atomic.Int64
	// exhausted counts the calls refused for the cap.
	exhausted atomic.Int64
}

type budgetState struct {
	Month    string  `json:"month"`
	SpentUSD float64 `json:"spent_usd"`
}

// NewBudget returns a budget with the cap monthlyUSD (0: none) and the prices of the model,
// kept in stateFile ("" keeps it in memory).
func NewBudget(monthlyUSD float64, prices Prices, stateFile string) (*Budget, error) {
	if monthlyUSD < 0 || math.IsNaN(monthlyUSD) || math.IsInf(monthlyUSD, 0) {
		return nil, fmt.Errorf("budget: invalid monthly cap %v", monthlyUSD)
	}
	if err := prices.validate(); err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	if stateFile != "" {
		if !filepath.IsAbs(stateFile) {
			return nil, fmt.Errorf("budget: state file %q must be an absolute path", stateFile)
		}
		if err := checkFlock(); err != nil {
			return nil, err
		}
	}
	return &Budget{MonthlyUSD: monthlyUSD, Prices: prices, state: stateFile, now: time.Now}, nil
}

func (b *Budget) month() string {
	return b.now().UTC().Format("2006-01")
}

// update applies fn to the state of the current month, under the lock, and stores the result
// when fn succeeds.
func (b *Budget) update(fn func(st *budgetState) error) (budgetState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	month := b.month()

	if b.state == "" {
		st := b.mem
		if st.Month != month {
			st = budgetState{Month: month}
		}
		if err := fn(&st); err != nil {
			return st, err
		}
		b.mem = st
		return st, nil
	}

	unlock, err := lockFile(b.state + ".lock")
	if err != nil {
		return budgetState{}, err
	}
	defer unlock()
	st, err := readBudgetState(b.state)
	if err != nil {
		return st, err
	}
	if st.Month != month {
		st = budgetState{Month: month}
	}
	if err := fn(&st); err != nil {
		return st, err
	}
	return st, writeBudgetState(b.state, st)
}

func readBudgetState(file string) (budgetState, error) {
	var st budgetState
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("budget: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("budget: state file %s: %w", file, err)
	}
	if st.SpentUSD < 0 || math.IsNaN(st.SpentUSD) || math.IsInf(st.SpentUSD, 0) {
		return st, fmt.Errorf("budget: state file %s: invalid spend %v", file, st.SpentUSD)
	}
	return st, nil
}

func writeBudgetState(file string, st budgetState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, filepath.Base(file)+".tmp-*")
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("budget: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("budget: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	return nil
}

// Reservation is the worst case of one call, booked until the call's usage replaces it.
type Reservation struct {
	b     *Budget
	month string
	usd   float64
}

// Reserve books the worst case of a call with at most inputTokens of input and maxOutputTokens of
// output. It fails with ErrBudgetExhausted, without booking anything, when the month's spend plus
// the worst case would exceed the cap.
func (b *Budget) Reserve(inputTokens, maxOutputTokens int) (*Reservation, error) {
	worst := b.Prices.cost(inputTokens, maxOutputTokens)
	st, err := b.update(func(st *budgetState) error {
		// The margin absorbs the rounding of sums of float prices (49 calls of $0.01 add up
		// to a little more than $0.49); it lets the spend pass the cap by a nano-dollar at most.
		if b.MonthlyUSD > 0 && st.SpentUSD+worst > b.MonthlyUSD+1e-9 {
			return ErrBudgetExhausted
		}
		st.SpentUSD += worst
		return nil
	})
	if errors.Is(err, ErrBudgetExhausted) {
		b.exhausted.Add(1)
		return nil, fmt.Errorf("%w: $%.4f spent of $%.2f this month, the next call may cost up to $%.4f", ErrBudgetExhausted, st.SpentUSD, b.MonthlyUSD, worst)
	}
	if err != nil {
		return nil, err
	}
	b.calls.Add(1)
	return &Reservation{b: b, month: st.Month, usd: worst}, nil
}

// Book replaces the reservation with the cost of the usage the provider reported. A usage with no
// tokens (the provider reports none, or the call failed) keeps the reservation. A reservation of
// a month that has ended stays in that month.
func (r *Reservation) Book(u llm.Usage) error {
	if r == nil || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return nil
	}
	actual := r.b.Prices.cost(u.InputTokens, u.OutputTokens)
	_, err := r.b.update(func(st *budgetState) error {
		if st.Month != r.month {
			return nil
		}
		st.SpentUSD = math.Max(0, st.SpentUSD+actual-r.usd)
		return nil
	})
	return err
}

// BudgetSnapshot is the state of a budget, for metrics and reports.
type BudgetSnapshot struct {
	Month      string
	SpentUSD   float64
	MonthlyUSD float64
	Calls      int64
	Exhausted  int64
}

// Snapshot returns the spend of the current month.
func (b *Budget) Snapshot() (BudgetSnapshot, error) {
	st, err := b.update(func(*budgetState) error { return errReadOnly })
	if err != nil && !errors.Is(err, errReadOnly) {
		return BudgetSnapshot{}, err
	}
	return BudgetSnapshot{Month: st.Month, SpentUSD: st.SpentUSD, MonthlyUSD: b.MonthlyUSD, Calls: b.calls.Load(), Exhausted: b.exhausted.Load()}, nil
}

var errReadOnly = errors.New("read only")

// String describes the spend for an explanation.
func (s BudgetSnapshot) String() string {
	limit := "no cap"
	if s.MonthlyUSD > 0 {
		limit = "cap $" + strconv.FormatFloat(s.MonthlyUSD, 'f', 2, 64)
	}
	return fmt.Sprintf("AI spend %s: $%.4f (%s)", s.Month, s.SpentUSD, limit)
}

// budgetProvider makes every call of p go through the budget.
type budgetProvider struct {
	p llm.Provider
	b *Budget
}

func (bp budgetProvider) Name() string { return bp.p.Name() }

// Complete reserves the worst case of the call, makes it, and books the usage. The input is
// bounded by its size in bytes (a token of the providers' byte-level tokenizers covers at least
// one byte), plus the schema, which a provider may also put into the prompt, and a margin for
// the message framing. The output is bounded by req.MaxTokens, which a budget requires.
func (bp budgetProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if req.MaxTokens <= 0 {
		return nil, errors.New("budget: a call without max_tokens has no worst case")
	}
	input := len(req.System) + len(req.Prompt) + 256
	if req.JSONSchema != nil {
		schema, err := json.Marshal(req.JSONSchema)
		if err != nil {
			return nil, err
		}
		input += 2 * len(schema)
	}
	res, err := bp.b.Reserve(input, req.MaxTokens)
	if err != nil {
		return nil, err
	}
	resp, err := bp.p.Complete(ctx, req)
	var usage llm.Usage
	if resp != nil {
		usage = resp.Usage
	}
	if bookErr := res.Book(usage); bookErr != nil && err == nil {
		return nil, bookErr
	}
	return resp, err
}
