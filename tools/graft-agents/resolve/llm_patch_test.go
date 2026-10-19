package resolve

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// llmRawModel marks a fake answer whose text is sent as it is, not wrapped by patchWrap.
const llmRawModel = "raw-text"

// llmRawAnswer is an answer whose text the fakes send unchanged.
func llmRawAnswer(text string) *llm.Response {
	return &llm.Response{Text: text, Model: llmRawModel}
}

var (
	patchNonceRE = regexp.MustCompile(`===== BEGIN PATCH ([0-9a-f]+) =====`)
	patchHeadRE  = regexp.MustCompile(`starts with the first (\d+) lines?`)
	patchTailRE  = regexp.MustCompile(`ends with the last (\d+) lines?`)
)

// patchFail fails the test, or panics without one (a fake provider has none).
func patchFail(t testing.TB, format string, args ...any) {
	if t == nil {
		panic(fmt.Sprintf(format, args...))
	}
	t.Helper()
	t.Fatalf(format, args...)
}

// patchSection returns the body of the data section title of a prompt with the nonce.
func patchSection(t testing.TB, prompt, title, nonce string) string {
	begin := "===== BEGIN " + title + " " + nonce + " =====\n"
	i := strings.LastIndex(prompt, begin)
	if i < 0 {
		patchFail(t, "prompt has no %s section", title)
	}
	body := prompt[i+len(begin):]
	j := strings.Index(body, "===== END "+title+" "+nonce+" =====")
	if j < 0 {
		patchFail(t, "section %s has no end", title)
	}
	return strings.TrimSuffix(body[:j], "\n\\ No newline at end of data\n")
}

// patchWrap answers a patch request as a well-behaved model: the head and tail context of the
// request's CODE around region, between the PATCHED CODE lines with the request's nonce.
func patchWrap(t testing.TB, req llm.Request, region string) string {
	m := patchNonceRE.FindStringSubmatch(req.Prompt)
	if m == nil {
		patchFail(t, "request has no PATCH section:\n%s", req.Prompt)
	}
	nonce := m[1]
	code := llmSplitLines([]byte(patchSection(t, req.Prompt, "CODE", nonce)))
	nHead, nTail := 0, 0
	if m := patchHeadRE.FindStringSubmatch(req.System); m != nil {
		nHead, _ = strconv.Atoi(m[1])
	}
	if m := patchTailRE.FindStringSubmatch(req.System); m != nil {
		nTail, _ = strconv.Atoi(m[1])
	}
	head := strings.Join(code[:nHead], "")
	tail := strings.Join(code[len(code)-nTail:], "")
	return "Applied.\n===== BEGIN PATCHED CODE " + nonce + " =====\n" + head + region + tail + "===== END PATCHED CODE " + nonce + " =====\n"
}

// patchFake answers each call i with the replacement answer(i) of the region, which llmFake wraps
// (patchWrapResponse), or with a raw answer as it is.
func patchFake(t testing.TB, answer func(i int, req llm.Request) (*llm.Response, error)) *llmFake {
	t.Helper()
	return &llmFake{answer: answer}
}

func TestLLMUnifiedDiff(t *testing.T) {
	for _, tc := range []struct {
		name, a, b string
		n          int
		want       string
	}{
		{"identical", "a\nb\n", "a\nb\n", 3, ""},
		{"one line changed", "a\nb\nc\n", "a\nX\nc\n", 3, "@@ -1,3 +1,3 @@\n a\n-b\n+X\n c\n"},
		{"context bound", "1\n2\n3\n4\n5\n6\n7\n", "1\n2\n3\nX\n5\n6\n7\n", 1, "@@ -3,3 +3,3 @@\n 3\n-4\n+X\n 5\n"},
		{"insertion", "a\nc\n", "a\nb\nc\n", 3, "@@ -1,2 +1,3 @@\n a\n+b\n c\n"},
		{"from empty", "", "x\n", 3, "@@ -0,0 +1 @@\n+x\n"},
		{"two hunks", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "X\n2\n3\n4\n5\n6\n7\n8\nY\n", 1, "@@ -1,2 +1,2 @@\n-1\n+X\n 2\n@@ -8,2 +8,2 @@\n 8\n-9\n+Y\n"},
		{"no newline at end", "a\nb", "a\nc", 3, "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := llmUnifiedDiff(llmSplitLines([]byte(tc.a)), llmSplitLines([]byte(tc.b)), tc.n)
			if got != tc.want {
				t.Fatalf("diff =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestLLMPatchRequestLayout: one call per region; the system message holds the worked example,
// the commit messages, the other regions and the directives last; the user message the PATCH,
// then the CODE with three lines of context on each side; no JSON is asked for.
func TestLLMPatchRequestLayout(t *testing.T) {
	base := numbered("line", 30)
	ours := strings.NewReplacer("line 5\n", "o5\n", "line 20\n", "o20\n").Replace(base)
	theirs := strings.NewReplacer("line 5\n", "t5\n", "line 20\n", "t20\n").Replace(base)
	p := patchFake(t, func(i int, _ llm.Request) (*llm.Response, error) { return llmFakeAnswer("merged\n"), nil })
	f := LLMFile{Path: "two.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs), OursMessage: "ours did this", TheirsMessage: "theirs did that"}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 || res.Hunks != 2 {
		t.Fatalf("calls %d, hunks %d: want one call per region", len(p.reqs), res.Hunks)
	}
	want := strings.NewReplacer("line 5\n", "merged\n", "line 20\n", "merged\n").Replace(base)
	if string(res.Content) != want {
		t.Fatalf("content:\n%s", res.Content)
	}

	req := p.reqs[0]
	nonce := patchNonceRE.FindStringSubmatch(req.Prompt)[1]
	if req.JSONSchema != nil {
		t.Fatal("a JSON schema was requested")
	}
	if got := patchSection(t, req.Prompt, "CODE", nonce); got != "line 2\nline 3\nline 4\no5\nline 6\nline 7\nline 8\n" {
		t.Fatalf("CODE = %q", got)
	}
	if got := patchSection(t, req.Prompt, "PATCH", nonce); got != "@@ -1,7 +1,7 @@\n line 2\n line 3\n line 4\n-line 5\n+t5\n line 6\n line 7\n line 8\n" {
		t.Fatalf("PATCH = %q", got)
	}
	if strings.Index(req.Prompt, "BEGIN PATCH "+nonce) > strings.Index(req.Prompt, "BEGIN CODE "+nonce) {
		t.Fatal("the PATCH must come before the CODE")
	}
	sys := req.System
	order := []string{"A worked example", "OURS COMMIT MESSAGE", "THEIRS COMMIT MESSAGE", "BEGIN OTHER CODE 2", "The task, for two.txt", `a line "===== BEGIN PATCHED CODE ` + nonce, "starts with the first 3 lines and ends with the last 3 lines"}
	last := -1
	for _, s := range order {
		i := strings.Index(sys, s)
		if i < 0 || i < last {
			t.Fatalf("system message misses %q or has it out of order:\n%s", s, sys)
		}
		last = i
	}
	if got := patchSection(t, sys, "OTHER CODE 2", nonce); got != "line 17\nline 18\nline 19\no20\nline 21\nline 22\nline 23\n" {
		t.Fatalf("other region = %q", got)
	}
}

func TestLLMPatchValidation(t *testing.T) {
	base := numbered("line", 20)
	ours := strings.Replace(base, "line 10\n", "o10\n", 1)
	theirs := strings.Replace(base, "line 10\n", "t10\n", 1)
	f := LLMFile{Path: "x.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}
	nonce := func(req llm.Request) string { return patchNonceRE.FindStringSubmatch(req.Prompt)[1] }
	block := func(req llm.Request, body string) *llm.Response {
		n := nonce(req)
		return llmRawAnswer("===== BEGIN PATCHED CODE " + n + " =====\n" + body + "===== END PATCHED CODE " + n + " =====\n")
	}
	const head, tail = "line 7\nline 8\nline 9\n", "line 11\nline 12\nline 13\n"

	for _, tc := range []struct {
		name   string
		answer func(req llm.Request) *llm.Response
		want   string // the region, or "" for a rejection
		reason string
	}{
		{"well formed", func(req llm.Request) *llm.Response { return block(req, head+"both\n"+tail) }, "both\n", ""},
		{"no block", func(llm.Request) *llm.Response { return llmRawAnswer("both\n") }, "", "has no patched code"},
		{"no end line", func(req llm.Request) *llm.Response {
			return llmRawAnswer("===== BEGIN PATCHED CODE " + nonce(req) + " =====\n" + head + "both\n" + tail)
		}, "", "has no end line"},
		{"wrong nonce", func(req llm.Request) *llm.Response {
			return llmRawAnswer("===== BEGIN PATCHED CODE 0000 =====\n" + head + "both\n" + tail + "===== END PATCHED CODE 0000 =====\n")
		}, "", "has no patched code"},
		{"head changed", func(req llm.Request) *llm.Response { return block(req, "line 7\nline 8 changed\nline 9\nboth\n"+tail) }, "", "does not start with the context"},
		{"tail missing", func(req llm.Request) *llm.Response { return block(req, head+"both\n") }, "", "does not end with the context"},
		{"not newline terminated", func(req llm.Request) *llm.Response { return block(req, head+"both"+"\n"+tail[:0]+"x") }, "", "does not end with the context"},
		{"reads as a patch", func(req llm.Request) *llm.Response { return block(req, head+"-line 10\n+both\n"+tail) }, "", "reads as a patch"},
		{"conflict markers", func(req llm.Request) *llm.Response { return block(req, head+"<<<<<<< ours\nboth\n"+tail) }, "", "conflict markers"},
		{"equal to ours is kept", func(req llm.Request) *llm.Response { return block(req, head+"o10\n"+tail) }, "o10\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) { return tc.answer(req), nil }}
			res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
			if tc.want == "" {
				if !errors.Is(err, ErrLLMRejected) || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("err = %v, want a rejection: %s", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Replace(base, "line 10\n", tc.want, 1); string(res.Content) != want {
				t.Fatalf("content:\n%s", res.Content)
			}
		})
	}
}

// TestLLMCheckPatchedBoundaries: a model that drops the empty lines at a boundary of the context
// gets them back, within the bound; a region at the end of a file without a final newline keeps
// it; the newline rule holds elsewhere.
func TestLLMCheckPatchedBoundaries(t *testing.T) {
	r := llmPatchRegion{head: "\n\nx\n", nHead: 3, tail: "y\n\n\n", nTail: 3}
	r.code = r.head + "o\n" + r.tail
	got, err := llmCheckPatched("\nx\nnew\ny\n\n", r, "o\n", "t\n")
	if err != nil || got != "new\n" {
		t.Fatalf("got %q, %v: want the blank lines given back", got, err)
	}
	if _, err := llmCheckPatched("x\nnew\ny\n", llmPatchRegion{head: "\n\n\nx\n", nHead: 4, tail: "y\n", nTail: 1}, "o\n", "t\n"); err == nil {
		t.Fatal("three dropped blank lines with four context lines accepted, the bound is three")
	}

	end := llmPatchRegion{head: "a\n", nHead: 1}
	end.code = end.head + "o"
	if got, err := llmCheckPatched("a\nnew", end, "o", "t"); err != nil || got != "new" {
		t.Fatalf("got %q, %v: want a last line without a newline kept", got, err)
	}
	mid := llmPatchRegion{head: "a\n", nHead: 1}
	if _, err := llmCheckPatched("a\nnew", mid, "o\n", "t\n"); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("err = %v, want the newline rule", err)
	}
}

// TestLLMPatchAllOrNothing: the first rejected region rejects the file; no partial result.
func TestLLMPatchAllOrNothing(t *testing.T) {
	base := numbered("line", 30)
	ours := strings.NewReplacer("line 5\n", "o5\n", "line 20\n", "o20\n").Replace(base)
	theirs := strings.NewReplacer("line 5\n", "t5\n", "line 20\n", "t20\n").Replace(base)
	p := patchFake(t, func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 1 {
			return llmRawAnswer("no block"), nil
		}
		return llmFakeAnswer("merged\n"), nil
	})
	_, err := ResolveLLM(context.Background(), p, LLMFile{Path: "x.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}, LLMOptions{})
	if !errors.Is(err, ErrLLMRejected) || !strings.Contains(err.Error(), "region 2") {
		t.Fatalf("err = %v, want region 2 rejected", err)
	}
}

func TestLLMPatchBounds(t *testing.T) {
	// 21 conflicting regions: over the default of 20, declined without a call.
	base := numbered("line", 200)
	var ours, theirs = base, base
	for i := 1; i <= 21; i++ {
		l := "line " + strconv.Itoa(i*9) + "\n"
		ours = strings.Replace(ours, l, "o"+l, 1)
		theirs = strings.Replace(theirs, l, "t"+l, 1)
	}
	p := patchFake(t, func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("x\n"), nil })
	_, err := ResolveLLM(context.Background(), p, LLMFile{Path: "x.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}, LLMOptions{})
	if !errors.Is(err, ErrLLMNotApplicable) || len(p.reqs) != 0 {
		t.Fatalf("err = %v, calls %d: want declined without a call", err, len(p.reqs))
	}

	// A request over max_prompt_bytes declines the file before any call.
	f := LLMFile{Path: "x.txt", Base: []byte("a\n"), Ours: []byte(strings.Repeat("o", 5000) + "\n"), Theirs: []byte("t\n")}
	_, err = ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 4096})
	if !errors.Is(err, ErrLLMNotApplicable) || len(p.reqs) != 0 {
		t.Fatalf("err = %v, calls %d", err, len(p.reqs))
	}
}
