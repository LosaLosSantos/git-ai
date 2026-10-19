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
	"unicode/utf8"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

const (
	defaultLLMMaxPromptBytes = 64 << 10
	// The context lines of a region, before and after it, as synthmerge's default.
	defaultLLMContextLines   = 3
	defaultLLMMaxAnswerBytes = 1 << 20
	defaultLLMMaxRegions     = 20
	defaultLLMMaxDiffCells   = 8 << 20
	// llmMaxMessageBytes bounds each commit message put in a prompt. A longer
	// message is cut, and the prompt says so.
	llmMaxMessageBytes = 4 << 10
)

var (
	// ErrLLMNotApplicable means the llm strategy cannot take the file (a side
	// is absent, the content is binary, or the diff is too large). The
	// strategy chain continues with the next strategy.
	ErrLLMNotApplicable = errors.New("resolve: llm strategy not applicable")

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
	return o, nil
}

// LLMFile is the material for one conflicted file. Ours and Theirs must be
// non-nil for a content conflict (an empty file is []byte{}, an absent side
// is nil). Base is nil when the file was added on both sides.
//
// OursMessage and TheirsMessage are the commit messages of the two sides (the
// tip the change lands on, and the change), given to the model as the intent of
// each side. They are untrusted data like the contents, and each is cut to
// 4 KiB. Either may be empty.
type LLMFile struct {
	Path          string
	Base          []byte
	Ours          []byte
	Theirs        []byte
	OursMessage   string
	TheirsMessage string
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

	return llmResolvePatch(ctx, p, f, opt, nonce)
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
func llmMessages(sb *strings.Builder, nonce string, f LLMFile) {
	for _, m := range []struct{ title, text string }{
		{"OURS COMMIT MESSAGE", f.OursMessage},
		{"THEIRS COMMIT MESSAGE", f.TheirsMessage},
	} {
		if m.text == "" {
			continue
		}
		text := llmClip(m.text, llmMaxMessageBytes)
		if len(text) < len(m.text) {
			m.title += " (cut to the first 4 KiB)"
		}
		llmSection(sb, nonce, m.title, text, false)
	}
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
