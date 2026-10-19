package resolve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

const (
	defaultLLMMaxPromptBytes = 64 << 10
	// The context lines of a region, before and after it, as synthmerge's default.
	defaultLLMContextLines   = 3
	defaultLLMMaxAnswerBytes = 1 << 20
	defaultLLMMaxRegions     = 20
	// defaultLLMContextBytes caps the symmetric context of a region (OURS' change; the messages get
	// a quarter each): about a thousand tokens, so that the context of a conflict stays in step with
	// the PATCH and the CODE it explains.
	defaultLLMContextBytes = 4 << 10
	// defaultLLMCallTimeout bounds one model call; the strategy's timeout bounds the whole file.
	defaultLLMCallTimeout  = 120 * time.Second
	defaultLLMMaxDiffCells = 8 << 20
	// llmMaxMessageBytes bounds each commit message put in a prompt. A longer
	// message is cut, and the prompt says so.
	llmMaxMessageBytes = 4 << 10
)

var (
	// ErrLLMNotApplicable means the llm strategy cannot take the file (a side
	// is absent, the content is binary, or the diff is too large). The
	// strategy chain continues with the next strategy.
	ErrLLMNotApplicable = errors.New("resolve: llm strategy not applicable")

	// ErrLLMAbstained means the model answered that the two changes cannot both hold, and said why
	// (the design step, PLAN10 15.10). It is no failure and no guess: the agent gets the reason.
	ErrLLMAbstained = errors.New("resolve: llm abstained")

	// ErrLLMRejected means the model answered but the answer failed
	// validation (no patched code, the context lines, the newline, conflict
	// markers, size).
	ErrLLMRejected = errors.New("resolve: llm answer rejected")
)

// LLMOptions bounds and tunes the llm strategy.
type LLMOptions struct {
	// MaxPromptBytes bounds the request of one region (system and user
	// message). A file with a larger one is declined. Zero means the
	// default.
	MaxPromptBytes int
	// ContextLines is the number of lines of context of the PATCH and the
	// CODE, before and after each conflicting region. Zero means the
	// default.
	ContextLines int
	// MaxAnswerBytes bounds one answer. Zero means the default.
	MaxAnswerBytes int
	// MaxRegions bounds the conflicting regions of a file, which cost one
	// model call each. A file with more is declined. Zero means the default.
	MaxRegions int
	// MaxTokens is the answer token limit sent with every request. Zero leaves
	// the limit to the provider (its configured default), so that models with
	// reasoning need no tuning here.
	MaxTokens int
	// CallTimeout bounds one model call (one region), within the deadline of
	// the whole file that the caller's context carries. Zero means the
	// default.
	CallTimeout time.Duration
	// NoSymmetricContext leaves out OURS' side of the context: its change from the BASE as a diff
	// and its commit message. Without them the request is the one of R1b, which the measurement of
	// the design step (PLAN10 15.10) takes as its baseline.
	NoSymmetricContext bool
	// NoAbstain does not offer the model the answer "the two changes cannot both hold".
	NoAbstain bool
	// NoRepair makes no second call: an answer that the deterministic checks reject is rejected.
	NoRepair bool
	// MaxContextBytes caps the symmetric context of a region. Zero means the default.
	MaxContextBytes int
}

func (o LLMOptions) withDefaults() (LLMOptions, error) {
	if o.MaxTokens < 0 {
		return o, fmt.Errorf("resolve: invalid llm max tokens %d", o.MaxTokens)
	}
	if o.MaxPromptBytes <= 0 {
		o.MaxPromptBytes = defaultLLMMaxPromptBytes
	}
	if o.ContextLines <= 0 {
		o.ContextLines = defaultLLMContextLines
	}
	if o.MaxAnswerBytes <= 0 {
		o.MaxAnswerBytes = defaultLLMMaxAnswerBytes
	}
	if o.MaxRegions <= 0 {
		o.MaxRegions = defaultLLMMaxRegions
	}
	if o.MaxContextBytes <= 0 {
		o.MaxContextBytes = defaultLLMContextBytes
	}
	if o.CallTimeout < 0 {
		return o, fmt.Errorf("resolve: invalid llm call timeout %s", o.CallTimeout)
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = defaultLLMCallTimeout
	}
	return o, nil
}

// LLMFile is the material for one conflicted file. Ours and Theirs must be
// non-nil for a content conflict (an empty file is []byte{}, an absent side
// is nil). Base is nil when the file was added on both sides.
//
// TheirsMessage is the commit message of the change being landed, given to the
// model as the intent of the PATCH, as synthmerge sends the message of the
// commit it applies. It is untrusted data like the contents, cut to 4 KiB, and
// may be empty.
type LLMFile struct {
	Path          string
	Base          []byte
	Ours          []byte
	Theirs        []byte
	TheirsMessage string
	// OursMessage is the commit message that explains OURS' change to this file (the newest commit
	// of the tip's side that touched it), the intent of OURS as TheirsMessage is THEIRS'. Untrusted
	// data, cut, and may be empty.
	OursMessage string
}

// LLMResult is a validated model resolution of one file. Nothing the model
// says about its own answer decides whether it is used: the answer is accepted
// on the checks alone (markers, size, and the checks the chain runs after).
type LLMResult struct {
	// Content is the complete resolved file.
	Content []byte
	// Explanation joins the model's explanations.
	Explanation string
	// Model is the model that answered (or the provider name).
	Model string
	// Hunks is the number of conflicting regions the model answered: zero
	// for a file whose changes merged without overlap.
	Hunks int
	// RegionsEqualOurs is the number of those regions whose answer is OURS'
	// region unchanged.
	RegionsEqualOurs int
	// Report is the composition report of the answer (PLAN10 15.3), in exact words: what it keeps,
	// drops and repeats of the two changes, and how many contested regions it resolved freely. It is
	// a report and not a gate, and goes to the agent with the proposal.
	Report []string
	// Calls and Repairs are the model calls of the file and those of them that were repair rounds.
	Calls, Repairs int
}

// LLMRegionStats receives, from the llm strategy, the counts of the last file
// it resolved: the conflicting regions the model answered, and those whose
// answer is OURS' region unchanged. The bench puts one in the context of a
// case (WithLLMRegionStats) to count the answers equal to OURS per region, as
// synthmerge's no-change check does, besides per file.
type LLMRegionStats struct {
	mu                 sync.Mutex
	regions, equalOurs int
	recorded           bool
	rejected           []string
	usage              llm.Usage
	calls, repairs     int
	abstained          string
	report             []string
}

// RecordCalls adds the model calls of a file and those of them that were repair rounds.
func (s *LLMRegionStats) RecordCalls(calls, repairs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls += calls
	s.repairs += repairs
}

// Calls returns the model calls recorded and the repair rounds among them.
func (s *LLMRegionStats) Calls() (calls, repairs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.repairs
}

// RecordAbstained keeps why the model abstained.
func (s *LLMRegionStats) RecordAbstained(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abstained = reason
}

// Abstained returns why the model abstained, or "".
func (s *LLMRegionStats) Abstained() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abstained
}

// RecordReport keeps the composition report of the last file resolved.
func (s *LLMRegionStats) RecordReport(report []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report = append([]string(nil), report...)
}

// Report returns the composition report of the last file resolved.
func (s *LLMRegionStats) Report() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.report...)
}

// RecordUsage adds the token usage of one model call, answered or rejected.
func (s *LLMRegionStats) RecordUsage(u llm.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.InputTokens += u.InputTokens
	s.usage.OutputTokens += u.OutputTokens
	s.usage.ReasoningTokens += u.ReasoningTokens
}

// Usage returns the token usage of the model calls recorded.
func (s *LLMRegionStats) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// RecordRejected keeps the raw text of a model answer that was rejected, with why, for audit.
func (s *LLMRegionStats) RecordRejected(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected = append(s.rejected, text)
}

// Rejected returns the raw texts of the rejected answers, in order.
func (s *LLMRegionStats) Rejected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rejected...)
}

// Record sets the counts of the last file resolved.
func (s *LLMRegionStats) Record(regions, equalOurs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regions, s.equalOurs, s.recorded = regions, equalOurs, true
}

// Get returns the counts of the last file resolved, and false when none was.
func (s *LLMRegionStats) Get() (regions, equalOurs int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.regions, s.equalOurs, s.recorded
}

type llmRegionStatsKey struct{}

// WithLLMRegionStats returns ctx carrying s, for the llm strategy to record into.
func WithLLMRegionStats(ctx context.Context, s *LLMRegionStats) context.Context {
	return context.WithValue(ctx, llmRegionStatsKey{}, s)
}

// llmRegionStatsFrom returns the LLMRegionStats of ctx, or nil.
func llmRegionStatsFrom(ctx context.Context) *LLMRegionStats {
	s, _ := ctx.Value(llmRegionStatsKey{}).(*LLMRegionStats)
	return s
}

// ResolveLLM asks p to resolve one conflicted file: region by region, by applying THEIRS' change
// to OURS' code (llm_patch.go). The regions that only one side changed, or both the same way, are
// merged locally without the model. Every answer is validated (the context lines, the newline,
// conflict markers, size) before it is used, and the file is resolved only when every region is.
// The caller still runs the resolve checks on the result.
//
// The llm Strategy adapter (llm_strategy.go) converts a strategy File into an LLMFile and maps the
// errors below onto the chain semantics.
func ResolveLLM(ctx context.Context, p llm.Provider, f LLMFile, opt LLMOptions) (LLMResult, error) {
	if p == nil {
		return LLMResult{}, errors.New("resolve: llm strategy has no provider")
	}
	// Text that is not valid UTF-8 cannot reach the model unchanged: the
	// providers JSON-encode the prompt, which replaces every invalid byte with
	// U+FFFD. The model could then not give those bytes back, so such a file is
	// declined rather than silently corrupted.
	if f.Ours == nil || f.Theirs == nil || llmIsBinary(f.Base, f.Ours, f.Theirs) || !llmIsUTF8(f.Base, f.Ours, f.Theirs) {
		return LLMResult{}, ErrLLMNotApplicable
	}

	opt, err := opt.withDefaults()
	if err != nil {
		return LLMResult{}, err
	}

	// The nonce delimits the file data in the prompt. File content is
	// untrusted, so it cannot close the delimiters it cannot know.
	nonce, err := llmNonce()
	if err != nil {
		return LLMResult{}, err
	}

	// A file whose sides all end their lines with CRLF reaches the model with LF: a model answers
	// with LF, and the context lines must come back exactly. The answer is read as LF (CRLF in
	// it is taken as LF too) and the result is CRLF again. A file with mixed line endings is
	// sent as it is.
	crlf := LLMLineEndings(f.Base, f.Ours, f.Theirs) == LineEndingsCRLF
	if crlf {
		f.Base, f.Ours, f.Theirs = llmToLF(f.Base), llmToLF(f.Ours), llmToLF(f.Theirs)
	}
	res, err := llmResolvePatch(ctx, p, f, opt, nonce, crlf)
	if err != nil {
		return LLMResult{}, err
	}
	if crlf {
		res.Content = bytes.ReplaceAll(res.Content, []byte("\n"), []byte("\r\n"))
	}
	return res, nil
}

// The line endings of a file's sides (LLMLineEndings).
const (
	LineEndingsLF    = "lf"
	LineEndingsCRLF  = "crlf"
	LineEndingsMixed = "mixed"
)

// LLMLineEndings returns the line endings of the sides that are present (nil is absent): CRLF when
// every line break of every side is CRLF and there is one, LF when none is, and mixed otherwise.
func LLMLineEndings(sides ...[]byte) string {
	breaks, crlf := 0, 0
	for _, s := range sides {
		breaks += bytes.Count(s, []byte("\n"))
		crlf += bytes.Count(s, []byte("\r\n"))
	}
	switch {
	case crlf == 0:
		return LineEndingsLF
	case crlf == breaks:
		return LineEndingsCRLF
	}
	return LineEndingsMixed
}

// llmToLF returns b with CRLF line breaks as LF, keeping an absent side nil.
func llmToLF(b []byte) []byte {
	if b == nil {
		return nil
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// llmNonce returns a random token that marks the data sections of a prompt.
func llmNonce() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("resolve: llm nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// llmClip cuts s to at most n bytes on a UTF-8 boundary.
func llmClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func llmComplete(ctx context.Context, p llm.Provider, req llm.Request) (*llm.Response, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("resolve: llm: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: empty response", ErrLLMRejected)
	}
	return resp, nil
}

// llmCheckAnswer applies the size bound and the marker rule to the replacement of a region.
func llmCheckAnswer(content string, opt LLMOptions) error {
	reject := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrLLMRejected, fmt.Sprintf(format, args...))
	}

	switch {
	case len(content) > opt.MaxAnswerBytes:
		return reject("answer of %d bytes exceeds %d", len(content), opt.MaxAnswerBytes)
	case llmHasConflictMarkers(content):
		return reject("answer contains conflict markers")
	}
	return nil
}

// llmHasConflictMarkers reports whether a line starts with a Git conflict
// marker. It shares HasConflictMarkers, which follows the server-side answer
// validation, so an answer the server would accept is never rejected here.
func llmHasConflictMarkers(s string) bool {
	return HasConflictMarkers([]byte(s))
}

func llmIsBinary(bs ...[]byte) bool {
	for _, b := range bs {
		if bytes.IndexByte(b, 0) >= 0 {
			return true
		}
	}
	return false
}

// llmIsUTF8 reports whether every side is valid UTF-8. A nil side is valid.
func llmIsUTF8(bs ...[]byte) bool {
	for _, b := range bs {
		if !utf8.Valid(b) {
			return false
		}
	}
	return true
}

func llmModel(resp *llm.Response, p llm.Provider) string {
	if resp.Model != "" {
		return resp.Model
	}
	return p.Name()
}

// llmMessages writes the commit messages of both sides, each cut to
// llmMaxMessageBytes, as data sections. An empty message is left out. The
// messages count towards the prompt size like the file data.
func llmMessage(sb *strings.Builder, nonce, title, message string) {
	text := llmClip(message, llmMaxMessageBytes)
	if len(text) < len(message) {
		title += " (cut to the first 4 KiB)"
	}
	llmSection(sb, nonce, title, text, false)
}

// llmSection writes one data section delimited by BEGIN and END lines that
// carry the per-request nonce. The data is written verbatim: an empty section
// has no lines between BEGIN and END, and a side that is absent says so in the
// BEGIN line, so no file content can be mistaken for a marker. Data that lacks
// a final newline is followed by the git convention for that case, so the model
// does not add or drop the end-of-file newline by mistake.
func llmSection(sb *strings.Builder, nonce, title, data string, absent bool) {
	if absent {
		fmt.Fprintf(sb, "===== BEGIN %s %s (absent) =====\n===== END %s %s =====\n\n", title, nonce, title, nonce)
		return
	}
	fmt.Fprintf(sb, "===== BEGIN %s %s =====\n", title, nonce)
	sb.WriteString(data)
	if data != "" && !strings.HasSuffix(data, "\n") {
		sb.WriteString("\n\\ No newline at end of data\n")
	}
	fmt.Fprintf(sb, "===== END %s %s =====\n\n", title, nonce)
}

// llmSplitLines splits b into lines that keep their line terminator.
func llmSplitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(b, []byte("\n"))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// llmEdit replaces base lines [start, end) with repl.
type llmEdit struct {
	start, end int
	repl       []string
}

const (
	llmSideOurs = iota
	llmSideTheirs
)

type llmHunk struct {
	llmEdit
	side int
}

// llmRegion is a stretch of base lines [start, end) that one or both sides
// changed. A clean region is merged locally; a conflicting region is answered
// by the model.
type llmRegion struct {
	start, end int
	clean      bool
	merged     string // replacement text of a clean region

	base, ours, theirs string // texts of the region on each side
	answer             string // model replacement of a conflicting region
}

// llmDiff returns the line edits that turn a into b, computed with an LCS
// over the lines after trimming the common prefix and suffix. It fails when
// the table would exceed maxCells.
func llmDiff(a, b []string, maxCells int) ([]llmEdit, error) {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}

	am := a[pre : len(a)-suf]
	bm := b[pre : len(b)-suf]
	n, m := len(am), len(bm)
	if n == 0 && m == 0 {
		return nil, nil
	}

	w := m + 1
	if (n+1)*w > maxCells {
		return nil, fmt.Errorf("diff of %d and %d lines is too large", n, m)
	}

	// L[i*w+j] is the LCS length of am[i:] and bm[j:].
	L := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case am[i] == bm[j]:
				L[i*w+j] = L[(i+1)*w+j+1] + 1
			case L[(i+1)*w+j] >= L[i*w+j+1]:
				L[i*w+j] = L[(i+1)*w+j]
			default:
				L[i*w+j] = L[i*w+j+1]
			}
		}
	}

	var edits []llmEdit
	cur := -1
	i, j := 0, 0
	for i < n || j < m {
		if i < n && j < m && am[i] == bm[j] {
			cur = -1
			i++
			j++
			continue
		}
		if cur < 0 {
			edits = append(edits, llmEdit{start: pre + i, end: pre + i})
			cur = len(edits) - 1
		}
		if i < n && (j == m || L[(i+1)*w+j] >= L[i*w+j+1]) {
			i++
			edits[cur].end = pre + i
		} else {
			edits[cur].repl = append(edits[cur].repl, bm[j])
			j++
		}
	}
	return edits, nil
}

// llmRegions merges the edits of both sides into regions. Edits that touch
// or overlap in base coordinates form one region. A region is clean when
// one side left it unchanged or both sides produced the same text.
func llmRegions(base []string, ours, theirs []llmEdit) []llmRegion {
	hs := make([]llmHunk, 0, len(ours)+len(theirs))
	for _, e := range ours {
		hs = append(hs, llmHunk{llmEdit: e, side: llmSideOurs})
	}
	for _, e := range theirs {
		hs = append(hs, llmHunk{llmEdit: e, side: llmSideTheirs})
	}
	sort.SliceStable(hs, func(i, j int) bool {
		if hs[i].start != hs[j].start {
			return hs[i].start < hs[j].start
		}
		return hs[i].end < hs[j].end
	})

	var regions []llmRegion
	var group []llmHunk
	groupEnd := 0
	flush := func() {
		if len(group) > 0 {
			regions = append(regions, llmMakeRegion(base, group))
			group = nil
		}
	}
	for _, h := range hs {
		if len(group) > 0 && h.start > groupEnd {
			flush()
		}
		group = append(group, h)
		groupEnd = max(groupEnd, h.end)
	}
	flush()
	return regions
}

func llmMakeRegion(base []string, group []llmHunk) llmRegion {
	start, end := group[0].start, group[0].end
	for _, h := range group[1:] {
		end = max(end, h.end)
	}

	oursText := llmApply(base, start, end, group, llmSideOurs)
	theirsText := llmApply(base, start, end, group, llmSideTheirs)
	baseText := strings.Join(base[start:end], "")

	r := llmRegion{start: start, end: end, base: baseText, ours: oursText, theirs: theirsText}
	switch {
	case oursText == theirsText, theirsText == baseText:
		r.clean = true
		r.merged = oursText
	case oursText == baseText:
		r.clean = true
		r.merged = theirsText
	}
	return r
}

// llmApply returns the text of base [start, end) with the hunks of one side
// applied. The hunks must be sorted by start.
func llmApply(base []string, start, end int, hs []llmHunk, side int) string {
	var sb strings.Builder
	pos := start
	for _, h := range hs {
		if h.side != side {
			continue
		}
		sb.WriteString(strings.Join(base[pos:h.start], ""))
		sb.WriteString(strings.Join(h.repl, ""))
		pos = h.end
	}
	sb.WriteString(strings.Join(base[pos:end], ""))
	return sb.String()
}

// llmAssemble builds the resolved file from base and its regions.
func llmAssemble(base []string, regions []llmRegion) string {
	var sb strings.Builder
	pos := 0
	for i := range regions {
		r := &regions[i]
		sb.WriteString(strings.Join(base[pos:r.start], ""))
		if r.clean {
			sb.WriteString(r.merged)
		} else {
			sb.WriteString(r.answer)
		}
		pos = r.end
	}
	sb.WriteString(strings.Join(base[pos:], ""))
	return sb.String()
}
