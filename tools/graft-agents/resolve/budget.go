package resolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// Prices are the prices of a model in USD, as configured. OutputPerMillion applies to every
// output token, reasoning included (providers count reasoning tokens as output).
type Prices struct {
	InputPerMillion  float64
	OutputPerMillion float64
	// PerRequest is a fixed price per call, when the provider charges one.
	PerRequest float64
}

// prices are Prices in nano-dollars, converted exactly (usdToNano).
type prices struct {
	inputPerMillion, outputPerMillion, perRequest nanoUSD
}

func (p Prices) exact() (prices, error) {
	var out prices
	for _, f := range []struct {
		usd float64
		dst *nanoUSD
	}{{p.InputPerMillion, &out.inputPerMillion}, {p.OutputPerMillion, &out.outputPerMillion}, {p.PerRequest, &out.perRequest}} {
		n, err := usdToNano(f.usd)
		if err != nil {
			return prices{}, fmt.Errorf("invalid price: %w", err)
		}
		*f.dst = n
	}
	if out == (prices{}) {
		return prices{}, errors.New("no price is set")
	}
	return out, nil
}

// cost returns the cost of a call with the given tokens, rounded up.
func (p prices) cost(inputTokens, outputTokens int) (nanoUSD, error) {
	in, err := tokenCost(inputTokens, p.inputPerMillion)
	if err != nil {
		return 0, err
	}
	out, err := tokenCost(outputTokens, p.outputPerMillion)
	if err != nil {
		return 0, err
	}
	return addNano(in, out, p.perRequest)
}

// Budget tracks the AI spend of a calendar month (UTC) and caps it (PLAN10 3.5). Before every
// model call the worst case of the call is reserved: the spend is raised by it, and the call is
// not made when that would exceed the cap. After the call the reservation is replaced by the
// cost of the usage the provider reports. So the recorded spend is never below the real one, and
// a process that dies between the two steps leaves its reservation booked.
//
// Money is counted in whole nano-dollars: the prices and the cap are converted exactly from
// their decimal form, a call's cost is rounded up, and sums and the comparison with the cap are
// exact.
//
// With a state file the spend survives restarts and is shared by every graft-resolve process
// that uses the file: each update holds an exclusive lock (flock on state + ".lock"), reads the
// file, and writes it again atomically (temporary file, fsync, rename, fsync of the directory).
// Without a state file the spend lives in memory (the benchmark).
type Budget struct {
	// MonthlyUSD is the cap as configured; 0 means no cap (the spend is still tracked).
	MonthlyUSD float64
	Prices     Prices

	monthly nanoUSD
	prices  prices
	state   string
	now     func() time.Time

	mu    sync.Mutex
	mem   budgetState
	calls atomic.Int64
	// exhausted counts the calls refused for the cap.
	exhausted atomic.Int64
	// paymentRefusals counts the calls the provider refused with HTTP 402.
	paymentRefusals atomic.Int64
}

// budgetState is the content of the state file: {"month": "2026-10", "spent_usd": 1.250000000},
// the spend written as an exact decimal number.
type budgetState struct {
	Month string
	Spent nanoUSD
}

type budgetStateJSON struct {
	Month    string      `json:"month"`
	SpentUSD json.Number `json:"spent_usd"`
}

func (st budgetState) MarshalJSON() ([]byte, error) {
	return json.Marshal(budgetStateJSON{Month: st.Month, SpentUSD: json.Number(st.Spent.String())})
}

func (st *budgetState) UnmarshalJSON(data []byte) error {
	var w budgetStateJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return err
	}
	spent, err := parseNanoUSD(w.SpentUSD.String())
	if err != nil {
		return fmt.Errorf("spent_usd: %w", err)
	}
	*st = budgetState{Month: w.Month, Spent: spent}
	return nil
}

// NewBudget returns a budget with the cap monthlyUSD (0: none) and the prices of the model,
// kept in stateFile ("" keeps it in memory).
func NewBudget(monthlyUSD float64, p Prices, stateFile string) (*Budget, error) {
	monthly, err := usdToNano(monthlyUSD)
	if err != nil {
		return nil, fmt.Errorf("budget: invalid monthly cap: %w", err)
	}
	exact, err := p.exact()
	if err != nil {
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
	return &Budget{MonthlyUSD: monthlyUSD, Prices: p, monthly: monthly, prices: exact, state: stateFile, now: time.Now}, nil
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
	worst nanoUSD
}

// Reserve books the worst case of a call with at most inputTokens of input and maxOutputTokens of
// output. It fails with ErrBudgetExhausted, without booking anything, when the month's spend plus
// the worst case would exceed the cap.
func (b *Budget) Reserve(inputTokens, maxOutputTokens int) (*Reservation, error) {
	worst, err := b.prices.cost(inputTokens, maxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	st, err := b.update(func(st *budgetState) error {
		total, err := addNano(st.Spent, worst)
		if err != nil {
			return err
		}
		if b.monthly > 0 && total > b.monthly {
			return ErrBudgetExhausted
		}
		st.Spent = total
		return nil
	})
	if errors.Is(err, ErrBudgetExhausted) {
		b.exhausted.Add(1)
		return nil, fmt.Errorf("%w: $%s spent of $%s this month, the next call may cost up to $%s", ErrBudgetExhausted, st.Spent, b.monthly, worst)
	}
	if err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	b.calls.Add(1)
	return &Reservation{b: b, month: st.Month, worst: worst}, nil
}

// Book replaces the reservation with the cost of the usage the provider reported. A usage with no
// tokens (the provider reports none, or the call failed) keeps the reservation. A reservation of
// a month that has ended stays in that month.
func (r *Reservation) Book(u llm.Usage) error {
	if r == nil || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return nil
	}
	actual, err := r.b.prices.cost(u.InputTokens, u.OutputTokens)
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	_, err = r.b.update(func(st *budgetState) error {
		if st.Month != r.month {
			return nil
		}
		// The worst case was booked; the reported usage replaces it. A usage above the worst case
		// (a provider that passed max_tokens) is booked in full.
		spent, err := addNano(st.Spent-min(st.Spent, r.worst), actual)
		if err != nil {
			return err
		}
		st.Spent = spent
		return nil
	})
	return err
}

// Release removes the reservation of a call that the provider refused before generating anything
// with a status it does not bill (llm.Unbilled): it cost nothing. A reservation of a month that
// has ended stays in that month.
func (r *Reservation) Release() error {
	if r == nil {
		return nil
	}
	_, err := r.b.update(func(st *budgetState) error {
		if st.Month != r.month {
			return nil
		}
		st.Spent -= min(st.Spent, r.worst)
		return nil
	})
	return err
}

// BudgetSnapshot is the state of a budget, for metrics and reports.
type BudgetSnapshot struct {
	Month string
	// SpentUSD and MonthlyUSD are for display; SpentNanoUSD is exact.
	SpentUSD     float64
	SpentNanoUSD int64
	MonthlyUSD   float64
	Calls        int64
	Exhausted    int64
}

// Snapshot returns the spend of the current month.
func (b *Budget) Snapshot() (BudgetSnapshot, error) {
	st, err := b.update(func(*budgetState) error { return errReadOnly })
	if err != nil && !errors.Is(err, errReadOnly) {
		return BudgetSnapshot{}, err
	}
	return BudgetSnapshot{
		Month: st.Month, SpentUSD: st.Spent.USD(), SpentNanoUSD: int64(st.Spent), MonthlyUSD: b.monthly.USD(),
		Calls: b.calls.Load(), Exhausted: b.exhausted.Load(),
	}, nil
}

var errReadOnly = errors.New("read only")

// String describes the spend for an explanation.
func (s BudgetSnapshot) String() string {
	limit := "no cap"
	if s.MonthlyUSD > 0 {
		limit = fmt.Sprintf("cap $%.2f", s.MonthlyUSD)
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
	if status, ok := llm.HTTPStatus(err); ok && status == http.StatusPaymentRequired {
		bp.b.paymentRefusals.Add(1)
	}
	// A refusal before generation that the provider does not bill releases the worst case; any
	// other failure keeps it, as it may have been billed.
	if err != nil && llm.Unbilled(err) {
		if relErr := res.Release(); relErr != nil {
			return nil, errors.Join(err, relErr)
		}
		return nil, err
	}
	var usage llm.Usage
	if resp != nil {
		usage = resp.Usage
	}
	if bookErr := res.Book(usage); bookErr != nil && err == nil {
		return nil, bookErr
	}
	return resp, err
}

// PaymentRefusals returns the number of calls the provider refused with HTTP 402 (no credit), of
// any kind. A run that meets one should stop: the calls after it fail the same way.
func (b *Budget) PaymentRefusals() int64 {
	return b.paymentRefusals.Load()
}
