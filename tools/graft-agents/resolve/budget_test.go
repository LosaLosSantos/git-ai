//go:build cgo

package resolve

import (
	"context"
	"encoding/json"
	"errors"

	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// wp4Prices costs $1 per million input tokens and $10 per million output tokens.
var wp4Prices = Prices{InputPerMillion: 1, OutputPerMillion: 10}

// wp4USD returns an exact amount written in dollars.
func wp4USD(t *testing.T, usd string) nanoUSD {
	t.Helper()
	n, err := parseNanoUSD(usd)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// wp4Spent returns the exact spend of the month.
func wp4Spent(t *testing.T, b *Budget) nanoUSD {
	t.Helper()
	s, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return nanoUSD(s.SpentNanoUSD)
}

func TestBudgetReserveAndBook(t *testing.T) {
	b, err := NewBudget(1, wp4Prices, "")
	if err != nil {
		t.Fatal(err)
	}
	// Worst case: 100k input ($0.10) + 10k output ($0.10).
	r, err := b.Reserve(100_000, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := wp4Spent(t, b); got != wp4USD(t, "0.2") {
		t.Fatalf("spent after reserve = %v, want the worst case 0.2", got)
	}
	if err := r.Book(llm.Usage{InputTokens: 30_000, OutputTokens: 2_000}); err != nil {
		t.Fatal(err)
	}
	if got := wp4Spent(t, b); got != wp4USD(t, "0.05") {
		t.Fatalf("spent after book = %v, want the real cost 0.05", got)
	}
}

func TestBudgetRefusesBeyondCap(t *testing.T) {
	b, _ := NewBudget(0.25, wp4Prices, "")
	if _, err := b.Reserve(100_000, 10_000); err != nil { // 0.20
		t.Fatal(err)
	}
	_, err := b.Reserve(100_000, 10_000) // 0.40 > 0.25
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if !strings.Contains(err.Error(), "AI budget exhausted") {
		t.Fatalf("message %q", err)
	}
	s, _ := b.Snapshot()
	if nanoUSD(s.SpentNanoUSD) != wp4USD(t, "0.2") || s.Exhausted != 1 || s.Calls != 1 {
		t.Fatalf("snapshot %+v: a refused call must book nothing", s)
	}
	// A smaller call still fits.
	if _, err := b.Reserve(10_000, 1_000); err != nil {
		t.Fatalf("small call refused: %v", err)
	}
}

func TestBudgetUnknownUsageKeepsReservation(t *testing.T) {
	b, _ := NewBudget(0, wp4Prices, "")
	r, _ := b.Reserve(100_000, 10_000)
	if err := r.Book(llm.Usage{}); err != nil {
		t.Fatal(err)
	}
	if got := wp4Spent(t, b); got != wp4USD(t, "0.2") {
		t.Fatalf("spent = %v, want the reservation kept", got)
	}
}

func TestBudgetNoCapTracksSpend(t *testing.T) {
	b, _ := NewBudget(0, wp4Prices, "")
	for i := 0; i < 100; i++ {
		if _, err := b.Reserve(1_000_000, 0); err != nil {
			t.Fatalf("call %d refused without a cap: %v", i, err)
		}
	}
	if got := wp4Spent(t, b); got != wp4USD(t, "100") {
		t.Fatalf("spent = %v", got)
	}
}

func TestBudgetStateFileSurvivesRestartAndIsShared(t *testing.T) {
	state := filepath.Join(t.TempDir(), "budget.json")
	a, err := NewBudget(1, wp4Prices, state)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := a.Reserve(100_000, 10_000)
	_ = r.Book(llm.Usage{InputTokens: 100_000, OutputTokens: 0}) // 0.10

	// Another process (or a restart) sees the spend.
	b, _ := NewBudget(1, wp4Prices, state)
	if got := wp4Spent(t, b); got != wp4USD(t, "0.1") {
		t.Fatalf("restarted budget spent = %v, want 0.1", got)
	}
	if _, err := b.Reserve(800_000, 0); err != nil { // 0.90 total
		t.Fatal(err)
	}
	if _, err := a.Reserve(200_000, 0); !errors.Is(err, ErrBudgetExhausted) { // 1.10 > 1
		t.Fatalf("err = %v: the other process's spend was not seen", err)
	}

	var st budgetState
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil || st.Month != time.Now().UTC().Format("2006-01") || st.Spent != wp4USD(t, "0.9") {
		t.Fatalf("state %s (%v)", data, err)
	}
	if matches, _ := filepath.Glob(state + ".tmp-*"); len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
}

func TestBudgetConcurrentReservationsNeverExceedCap(t *testing.T) {
	state := filepath.Join(t.TempDir(), "budget.json")
	// Two budgets on one file stand for two processes; each call's worst case is $0.01.
	budgets := make([]*Budget, 2)
	for i := range budgets {
		var err error
		if budgets[i], err = NewBudget(0.5, wp4Prices, state); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func(b *Budget) {
			defer wg.Done()
			if _, err := b.Reserve(10_000, 0); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(budgets[i%2])
	}
	wg.Wait()
	if granted != 50 {
		t.Fatalf("granted %d calls of $0.01 under a $0.50 cap, want 50", granted)
	}
	if got := wp4Spent(t, budgets[0]); got != wp4USD(t, "0.5") {
		t.Fatalf("spent = %v", got)
	}
}

func TestBudgetNewMonthStartsAtZero(t *testing.T) {
	b, _ := NewBudget(1, wp4Prices, filepath.Join(t.TempDir(), "budget.json"))
	now := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	r, err := b.Reserve(900_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC)
	if got := wp4Spent(t, b); got != 0 {
		t.Fatalf("new month spent = %v, want 0", got)
	}
	// A call reserved in October and booked in November leaves November alone.
	if err := r.Book(llm.Usage{InputTokens: 1, OutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if got := wp4Spent(t, b); got != 0 {
		t.Fatalf("November spent = %v after booking an October call", got)
	}
}

func TestNewBudgetRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		monthly float64
		prices  Prices
		state   string
	}{
		"negative cap":   {monthly: -1, prices: wp4Prices},
		"no price":       {monthly: 1},
		"negative price": {monthly: 1, prices: Prices{InputPerMillion: -1, OutputPerMillion: 1}},
		"relative state": {monthly: 1, prices: wp4Prices, state: "budget.json"},
	} {
		if _, err := NewBudget(tc.monthly, tc.prices, tc.state); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// wp4Provider answers with a fixed usage and counts its calls.
type wp4Provider struct {
	mu    sync.Mutex
	calls int
	usage llm.Usage
}

func (p *wp4Provider) Name() string { return "wp4:fake" }

func (p *wp4Provider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"content": "x\n", "explanation": "wp4"})
	return &llm.Response{Text: string(b), JSON: b, Usage: p.usage}, nil
}

func TestBudgetProviderReservesInputBytesAndMaxTokens(t *testing.T) {
	b, _ := NewBudget(0, Prices{InputPerMillion: 1e6}, "") // $1 per input token: the cost counts the tokens
	p := &wp4Provider{}
	bp := budgetProvider{p: p, b: b}
	if _, err := bp.Complete(context.Background(), llm.Request{Prompt: "abc"}); err == nil {
		t.Fatal("call without max_tokens accepted")
	}
	if _, err := bp.Complete(context.Background(), llm.Request{System: "s", Prompt: strings.Repeat("p", 1000), MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	// No usage reported: the reservation stays, 1 + 1000 bytes + 256 of framing.
	if got := wp4Spent(t, b); got != wp4USD(t, "1257") {
		t.Fatalf("reserved %v input tokens, want 1257", got)
	}
}

func TestBudgetProviderDoesNotCallWhenExhausted(t *testing.T) {
	b, _ := NewBudget(0.001, wp4Prices, "")
	p := &wp4Provider{usage: llm.Usage{InputTokens: 10, OutputTokens: 10}}
	bp := budgetProvider{p: p, b: b}
	_, err := bp.Complete(context.Background(), llm.Request{Prompt: "x", MaxTokens: 1_000_000})
	if !errors.Is(err, ErrBudgetExhausted) || p.calls != 0 {
		t.Fatalf("err = %v, calls = %d: want no call", err, p.calls)
	}
}

const wp4Config = `
[[strategy]]
type = "llm"
paths = ["*.txt"]

[strategy.llm]
type = "wp19-resolve-fake"
max_tokens = 1000

[strategy.llm.budget]
monthly_usd = 5
input_usd_per_million = 2
output_usd_per_million = 10.5
state = "STATE"
`

func TestLLMStrategyBudgetConfig(t *testing.T) {
	state := filepath.Join(t.TempDir(), "budget.json")
	wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		r := wp19Answer("merged\n")
		r.Usage = llm.Usage{InputTokens: 1000, OutputTokens: 100}
		return r, nil
	})
	r := wp19Build(t, strings.Replace(wp4Config, "STATE", state, 1))
	budgets := r.Budgets()
	b := budgets["llm"]
	if b == nil || b.MonthlyUSD != 5 || b.Prices != (Prices{InputPerMillion: 2, OutputPerMillion: 10.5}) {
		t.Fatalf("budgets %+v", budgets)
	}
	resp, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a.txt")))
	if err != nil {
		t.Fatal(err)
	}
	// 1000 × $2/M + 100 × $10.5/M = $0.00305.
	if got := wp4Spent(t, b); got != wp4USD(t, "0.00305") {
		t.Fatalf("spent = %v", got)
	}
	if !strings.Contains(resp.Explanation, "llm: AI spend ") || !strings.Contains(resp.Explanation, "cap $5.00") {
		t.Fatalf("explanation %q lacks the spend", resp.Explanation)
	}
}

func TestLLMStrategyBudgetExhaustedIsTheReason(t *testing.T) {
	state := filepath.Join(t.TempDir(), "budget.json")
	if err := os.WriteFile(state, []byte(`{"month":"`+time.Now().UTC().Format("2006-01")+`","spent_usd":4.9999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("merged\n"), nil
	})
	r := wp19Build(t, strings.Replace(wp4Config, "STATE", state, 1))
	_, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a.txt")))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v", err)
	}
	attempts := unresolved.Failures[0].Attempts
	if len(attempts) != 1 || !errors.Is(attempts[0].Err, ErrBudgetExhausted) {
		t.Fatalf("attempts %+v", attempts)
	}
	if !strings.Contains(strings.Join(unresolved.Failures[0].Reasons, " "), "AI budget exhausted") {
		t.Fatalf("reasons %v", unresolved.Failures[0].Reasons)
	}
	if fake.calls() != 0 {
		t.Fatalf("model called %d times past the cap", fake.calls())
	}
}

func TestLLMStrategyBudgetConfigRejects(t *testing.T) {
	state := filepath.Join(t.TempDir(), "budget.json")
	base := strings.Replace(wp4Config, "STATE", state, 1)
	for name, tc := range map[string]struct{ config, want string }{
		"no max_tokens":  {strings.Replace(base, "max_tokens = 1000\n", "", 1), "needs max_tokens"},
		"no state":       {strings.Replace(base, `state = "`+state+`"`+"\n", "", 1), "state is required"},
		"unknown key":    {base + "currency = \"eur\"\n", "unknown keys [currency]"},
		"negative price": {strings.Replace(base, "input_usd_per_million = 2", "input_usd_per_million = -2", 1), "non-negative number"},
		"string cap":     {strings.Replace(base, "monthly_usd = 5", `monthly_usd = "5"`, 1), "non-negative number"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := wp19BuildErr(tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResolverLLMMaxPromptTokens(t *testing.T) {
	r := wp19Build(t, wp19LLMConfig)
	if got, want := r.LLMMaxPromptTokens(), defaultLLMMaxPromptBytes+2*llmMaxMessageBytes+64<<10; got != want {
		t.Fatalf("bound = %d, want %d", got, want)
	}
	r = wp19Build(t, strings.Replace(wp19LLMConfig, "type = \"llm\"\n", "type = \"llm\"\nmax_prompt_bytes = 1000\n", 1))
	if got := r.LLMMaxPromptTokens(); got != 1000+2*llmMaxMessageBytes+64<<10 {
		t.Fatalf("bound with max_prompt_bytes 1000 = %d", got)
	}
	if n := wp19Build(t, "[[strategy]]\ntype = \"union\"\npaths = [\"*.txt\"]\n").LLMMaxPromptTokens(); n != 0 {
		t.Fatalf("bound without llm = %d", n)
	}
}

func TestResolverUseBudget(t *testing.T) {
	wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		res := wp19Answer("merged\n")
		res.Usage = llm.Usage{InputTokens: 1_000_000}
		return res, nil
	})
	r := wp19Build(t, wp19LLMConfig) // max_tokens = 512
	b, _ := NewBudget(0, wp4Prices, "")
	n, err := r.UseBudget(b)
	if err != nil || n != 1 {
		t.Fatalf("UseBudget = %d, %v", n, err)
	}
	if _, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a.txt"))); err != nil {
		t.Fatal(err)
	}
	if got := wp4Spent(t, b); got != wp4USD(t, "1") {
		t.Fatalf("spent = %v, want $1", got)
	}

	noLimit := wp19Build(t, "[[strategy]]\ntype = \"llm\"\n[strategy.llm]\ntype = \"wp19-resolve-fake\"\n")
	if _, err := noLimit.UseBudget(b); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("UseBudget without max_tokens: %v", err)
	}
}
