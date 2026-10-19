// Package resolve implements the graft-resolve conflict resolver: an ordered
// chain of strategies, each followed by per-glob checks, applied to every
// conflicted file of a protocol request independently.
//
// Chain semantics: strategies are tried in configuration order. A strategy
// either answers (ok=true), declines (ok=false) or fails (error); a failure
// is recorded and the chain continues. An answer is accepted only if it
// passes the built-in validation (action, mode, size, no conflict markers in
// text files) and every check whose glob matches the path. A rejected answer
// also lets the chain continue. If no strategy accepts a file, the request
// fails with an *UnresolvedError.
//
// Strategies that implement Recorder are told about every accepted answer
// (the rerere store uses this to learn resolutions). Recording happens per
// file as soon as the answer is accepted, so when another file of the same
// request stays unresolved the request fails but the recorded answers persist.
// This is deliberate: a later request with the same conflict reuses them.
//
// The LLM strategy is not part of this package's built-ins; it is registered
// by the llm backend glue through RegisterStrategy.
package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// DefaultCheckTimeout bounds one check command when the check sets no timeout.
const DefaultCheckTimeout = 2 * time.Minute

// DefaultResolveTimeout bounds one whole resolve request.
const DefaultResolveTimeout = 5 * time.Minute

// ErrInvalidRequest wraps every rejection of a malformed request.
var ErrInvalidRequest = errors.New("graft-resolve: invalid request")

// File is one conflicted path, as sent by the server.
type File = protocol.FileConflict

// Resolution is a strategy answer for one file. Action is protocol.ActionWrite
// (Mode and Content set) or protocol.ActionDelete.
type Resolution struct {
	Action  string
	Mode    string
	Content []byte
}

// Strategy resolves conflicts. Implementations must be safe for concurrent
// use: the HTTP mode resolves several requests at once.
type Strategy interface {
	// Resolve returns (answer, true, nil) when it resolved f, (_, false, nil)
	// when it does not apply, and an error when it failed.
	Resolve(ctx context.Context, f File, req protocol.Request) (Resolution, bool, error)
}

// syntaxChecked is implemented by the strategies whose answers always get the syntax check
// (syntax.CheckMerge): the llm strategy. The check runs before the configured checks, and
// rejects an answer that does not parse when both sides it merges parse.
type syntaxChecked interface {
	syntaxChecked()
}

// Recorder is implemented by strategies that want to learn accepted answers.
type Recorder interface {
	Record(ctx context.Context, f File, res Resolution) error
}

// StrategySpec is one [[strategy]] table of the configuration. Options holds
// the remaining keys of the table, so a registered strategy can read its own
// settings (for example an llm sub-table).
type StrategySpec struct {
	Name    string
	Type    string
	Paths   []string
	Options map[string]any
}

// StrategyFactory builds a strategy from its configuration table.
type StrategyFactory func(spec StrategySpec) (Strategy, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]StrategyFactory{}
)

// RegisterStrategy makes a strategy type available to the configuration. It
// panics on a duplicate type, like llm.Register.
func RegisterStrategy(typ string, factory StrategyFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[typ]; ok {
		panic("resolve: duplicate strategy type " + typ)
	}
	registry[typ] = factory
}

func lookupStrategy(typ string) (StrategyFactory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[typ]
	return f, ok
}

// StrategyTypes returns the registered strategy types, sorted.
func StrategyTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Check is one [[check]] table: Command runs for every accepted answer whose
// path matches Paths. "{file}" in Command is replaced by the location of the
// answer, a file written under a temporary directory and named "check" plus
// the path's extension when that is short and alphanumeric (see checkFileName).
// The repository path itself never reaches the command line, so a check may
// put {file} inside a shell string. A non-zero exit rejects the answer. Env
// lists extra environment variables to pass through; by default a check sees
// only the minimal environment.
type Check struct {
	Paths   []string
	Command []string
	Timeout time.Duration
	Env     []string
}

// Binding is one configured strategy with its path filter. Paths empty means
// every file.
type Binding struct {
	Name     string
	Strategy Strategy
	Paths    []string
}

// Resolver runs the configured chain.
type Resolver struct {
	bindings []Binding
	checks   []Check
	// Timeout bounds one whole Resolve call when the caller uses it.
	Timeout time.Duration
	// MaxRequestBytes bounds the size of one request body in the command and
	// HTTP modes. It defaults to protocol.DefaultMaxRequestBytes, which covers
	// the server's default limits; raise it when the server's limits are raised.
	MaxRequestBytes int64
	// Logf, when set, receives diagnostics (for example a failed Record).
	Logf func(format string, args ...any)
}

// NewResolver validates and assembles a resolver.
func NewResolver(bindings []Binding, checks []Check, timeout time.Duration) (*Resolver, error) {
	if len(bindings) == 0 {
		return nil, errors.New("resolve: no strategies configured")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("resolve: timeout must be positive, got %s", timeout)
	}
	for i, b := range bindings {
		if b.Strategy == nil {
			return nil, fmt.Errorf("resolve: strategy %d (%q) is nil", i, b.Name)
		}
		if !validName(b.Name) {
			return nil, fmt.Errorf("resolve: invalid strategy name %q", b.Name)
		}
		if err := validatePatterns(b.Paths); err != nil {
			return nil, fmt.Errorf("resolve: strategy %q: %w", b.Name, err)
		}
	}
	for i, c := range checks {
		if len(c.Paths) == 0 {
			return nil, fmt.Errorf("resolve: check %d has no paths", i)
		}
		if err := validatePatterns(c.Paths); err != nil {
			return nil, fmt.Errorf("resolve: check %d: %w", i, err)
		}
		if len(c.Command) == 0 || c.Command[0] == "" {
			return nil, fmt.Errorf("resolve: check %d has no command", i)
		}
		if c.Timeout < 0 {
			return nil, fmt.Errorf("resolve: check %d has a negative timeout", i)
		}
		if err := validateEnvNames(c.Env); err != nil {
			return nil, fmt.Errorf("resolve: check %d: %w", i, err)
		}
	}
	return &Resolver{
		bindings:        append([]Binding(nil), bindings...),
		checks:          append([]Check(nil), checks...),
		Timeout:         timeout,
		MaxRequestBytes: protocol.DefaultMaxRequestBytes,
	}, nil
}

// Failure is one file that no strategy resolved, with the reasons collected
// along the chain.
type Failure struct {
	Path    string
	Reasons []string
	// Attempts are the strategies that applied to the file and did not resolve
	// it, in chain order, with what happened. Reasons holds the same in text.
	Attempts []Attempt
}

// AttemptKind says why a strategy that applied to a file did not resolve it.
type AttemptKind string

const (
	// AttemptFailed: the strategy returned an error (for a model, the call or
	// its answer failed; errors.Is(Err, ErrBudgetExhausted) when the AI budget
	// is spent).
	AttemptFailed AttemptKind = "failed"
	// AttemptRejected: the result broke a rule that every result must keep
	// (conflict markers, mode, size, or its syntax), or a model answer broke
	// the rules of an answer (ErrLLMRejected).
	AttemptRejected AttemptKind = "rejected"
	// AttemptCheckFailed: a configured check rejected the result.
	AttemptCheckFailed AttemptKind = "check_failed"
)

// Attempt is one strategy that applied to a file without resolving it.
type Attempt struct {
	Strategy string
	Kind     AttemptKind
	Err      error
}

// ErrBudgetExhausted is returned by a strategy that does not call its model
// because the AI budget is spent.
var ErrBudgetExhausted = errors.New("AI budget exhausted")

// UnresolvedError reports files that stayed unresolved.
type UnresolvedError struct {
	Failures []Failure
	// Resolved are the paths of the request that were resolved; nothing lands, since the request
	// is answered as a whole.
	Resolved []string
}

// maxOutcomeReasonBytes bounds the reason of one file in an ErrorResponse, as the server's
// conflict package does.
const maxOutcomeReasonBytes = 512

// Response returns the refusal as the protocol's ErrorResponse, with the outcome of every file of
// the request: resolved, rejected (a strategy's answer failed a rule or a check) or failed.
func (e *UnresolvedError) Response() protocol.ErrorResponse {
	out := protocol.ErrorResponse{Error: e.Error()}
	for _, p := range e.Resolved {
		out.Files = append(out.Files, protocol.FileOutcome{Path: p, Outcome: protocol.OutcomeResolved})
	}
	for _, f := range e.Failures {
		outcome := protocol.OutcomeFailed
		for _, a := range f.Attempts {
			if a.Kind == AttemptRejected || a.Kind == AttemptCheckFailed {
				outcome = protocol.OutcomeRejected
			}
		}
		reason := strings.Join(f.Reasons, "; ")
		if len(reason) > maxOutcomeReasonBytes {
			reason = strings.ToValidUTF8(reason[:maxOutcomeReasonBytes], "")
		}
		out.Files = append(out.Files, protocol.FileOutcome{Path: f.Path, Outcome: outcome, Reason: reason})
	}
	return out
}

func (e *UnresolvedError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %s", f.Path, strings.Join(f.Reasons, "; ")))
	}
	return "graft-resolve: unresolved conflict: " + strings.Join(parts, " | ")
}

// Resolve answers one request. It returns *UnresolvedError when at least one
// file stays unresolved, and an error wrapping ErrInvalidRequest for a
// malformed request. Nothing is returned partially.
func (r *Resolver) Resolve(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	if req.Operation != "" && req.Operation != protocol.OperationResolve {
		return protocol.Response{}, fmt.Errorf("%w: operation %q is not %q", ErrInvalidRequest, req.Operation, protocol.OperationResolve)
	}
	if len(req.Resolution) > 0 {
		return protocol.Response{}, fmt.Errorf("%w: a resolve request carries no resolution", ErrInvalidRequest)
	}
	if err := validateRequest(req); err != nil {
		return protocol.Response{}, err
	}
	maxBytes := req.Limits.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = protocol.DefaultMaxFileBytes
	}

	resp := protocol.Response{
		Version: protocol.Version,
		Files:   make([]protocol.ResolvedFile, 0, len(req.Files)),
	}
	var used []string
	seen := map[string]bool{}
	var notes []string
	var failures []Failure

	for _, f := range req.Files {
		out, name, attempts, err := r.resolveFile(ctx, req, f, maxBytes)
		if err != nil {
			return protocol.Response{}, err
		}
		if name == "" {
			failures = append(failures, Failure{Path: f.Path, Reasons: attemptReasons(attempts), Attempts: attempts})
			continue
		}
		if !seen[name] {
			seen[name] = true
			used = append(used, name)
		}
		notes = append(notes, fmt.Sprintf("%s=%s", f.Path, name))
		resp.Files = append(resp.Files, out)
	}
	if len(failures) > 0 {
		resolved := make([]string, 0, len(resp.Files))
		for _, f := range resp.Files {
			resolved = append(resolved, f.Path)
		}
		return protocol.Response{}, &UnresolvedError{Failures: failures, Resolved: resolved}
	}

	resp.Resolver = resolverName(used)
	if len(notes) > 0 {
		resp.Explanation = "resolved " + strings.Join(notes, ", ")
	}
	// The month's AI spend of the strategies that answered, so that command mode reports it too.
	budgets := r.Budgets()
	for _, name := range used {
		if b := budgets[name]; b != nil {
			if snap, err := b.Snapshot(); err == nil {
				resp.Explanation += "; " + name + ": " + snap.String()
			}
		}
	}
	return resp, nil
}

// resolverNamePrefix starts the resolver name of every answer.
const resolverNamePrefix = "graft-resolve/"

// resolverName returns the resolver name of an answer that used the given
// strategies, in order. The server rejects a name longer than
// protocol.MaxResolverNameBytes, and each strategy name may take 64 bytes, so
// when the full list does not fit, the names that fit are kept and the rest
// are counted: "graft-resolve/a,b,+2". The explanation still names the
// strategy of every file.
func resolverName(used []string) string {
	if len(used) == 0 {
		return resolverNamePrefix + "none"
	}

	full := resolverNamePrefix + strings.Join(used, ",")
	if len(full) <= protocol.MaxResolverNameBytes {
		return full
	}

	for kept := len(used) - 1; kept > 0; kept-- {
		name := fmt.Sprintf("%s%s,+%d", resolverNamePrefix, strings.Join(used[:kept], ","), len(used)-kept)
		if len(name) <= protocol.MaxResolverNameBytes {
			return name
		}
	}
	return fmt.Sprintf("%s+%d", resolverNamePrefix, len(used))
}

// attemptReasons returns the text form of the attempts, the reasons of a
// Failure.
func attemptReasons(attempts []Attempt) []string {
	if len(attempts) == 0 {
		return []string{"no strategy resolved this conflict"}
	}
	reasons := make([]string, 0, len(attempts))
	for _, a := range attempts {
		switch a.Kind {
		case AttemptRejected:
			reasons = append(reasons, fmt.Sprintf("%s: rejected: %v", a.Strategy, a.Err))
		case AttemptCheckFailed:
			reasons = append(reasons, fmt.Sprintf("%s: check failed: %v", a.Strategy, a.Err))
		default:
			reasons = append(reasons, fmt.Sprintf("%s: %v", a.Strategy, a.Err))
		}
	}
	return reasons
}

// resolveFile runs the chain for one file. It returns the strategy name that
// accepted the answer, or an empty name and the attempts of the strategies
// that applied. err is set only when ctx ended.
func (r *Resolver) resolveFile(ctx context.Context, req protocol.Request, f File, maxBytes int64) (protocol.ResolvedFile, string, []Attempt, error) {
	var attempts []Attempt
	for _, b := range r.bindings {
		if err := ctx.Err(); err != nil {
			return protocol.ResolvedFile{}, "", nil, err
		}
		if !matchPaths(b.Paths, f.Path) {
			continue
		}
		res, ok, err := b.Strategy.Resolve(ctx, f, req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return protocol.ResolvedFile{}, "", nil, ctxErr
			}
			// A model answer that breaks the rules of an answer (ErrLLMRejected) is a rejected
			// result, not a failed call.
			kind := AttemptFailed
			if errors.Is(err, ErrLLMRejected) {
				kind = AttemptRejected
			}
			attempts = append(attempts, Attempt{Strategy: b.Name, Kind: kind, Err: err})
			continue
		}
		if !ok {
			continue
		}
		if err := checkResolution(res, maxBytes); err != nil {
			attempts = append(attempts, Attempt{Strategy: b.Name, Kind: AttemptRejected, Err: err})
			continue
		}
		if _, ok := b.Strategy.(syntaxChecked); ok && res.Action == protocol.ActionWrite && res.Mode != protocol.ModeSymlink {
			if _, err := syntax.CheckMerge(ctx, f.Path, res.Content, sideContent(f.Ours), sideContent(f.Theirs)); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return protocol.ResolvedFile{}, "", nil, ctxErr
				}
				attempts = append(attempts, Attempt{Strategy: b.Name, Kind: AttemptCheckFailed, Err: err})
				continue
			}
		}
		if err := r.runChecks(ctx, f.Path, res); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return protocol.ResolvedFile{}, "", nil, ctxErr
			}
			attempts = append(attempts, Attempt{Strategy: b.Name, Kind: AttemptCheckFailed, Err: err})
			continue
		}
		r.record(ctx, f, res)
		return protocol.ResolvedFile{
			Path:    f.Path,
			Action:  res.Action,
			Mode:    modeOf(res),
			Content: contentOf(res),
		}, b.Name, nil, nil
	}
	return protocol.ResolvedFile{}, "", attempts, nil
}

func modeOf(res Resolution) string {
	if res.Action == protocol.ActionDelete {
		return ""
	}
	return res.Mode
}

func contentOf(res Resolution) []byte {
	if res.Action == protocol.ActionDelete {
		return nil
	}
	return res.Content
}

func (r *Resolver) record(ctx context.Context, f File, res Resolution) {
	for _, b := range r.bindings {
		rec, ok := b.Strategy.(Recorder)
		if !ok {
			continue
		}
		if err := rec.Record(ctx, f, res); err != nil && r.Logf != nil {
			r.Logf("recording resolution of %s by %s failed: %v", f.Path, b.Name, err)
		}
	}
}

func (r *Resolver) runChecks(ctx context.Context, filePath string, res Resolution) error {
	if res.Action == protocol.ActionDelete {
		return nil
	}
	for _, c := range r.checks {
		if !matchPaths(c.Paths, filePath) {
			continue
		}
		if err := runCheck(ctx, c, filePath, res.Content); err != nil {
			return err
		}
	}
	return nil
}

func runCheck(ctx context.Context, c Check, filePath string, content []byte) error {
	dir, err := os.MkdirTemp("", "graft-resolve-check-")
	if err != nil {
		return fmt.Errorf("creating check directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	file := filepath.Join(dir, checkFileName(filePath))
	if err := os.WriteFile(file, content, 0o600); err != nil {
		return fmt.Errorf("writing check input: %w", err)
	}
	argv := substitute(c.Command, map[string]string{"{file}": file})
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultCheckTimeout
	}
	_, stderr, err := execute(ctx, execSpec{dir: dir, argv: argv, timeout: timeout, env: c.Env})
	if err != nil {
		return withStderr(fmt.Errorf("%s: %w", argv[0], err), stderr)
	}
	return nil
}

// checkFileName is the base name a check sees for an answer. The repository
// path is attacker-influenced content, so it is never used as is: a check that
// runs "sh -c" with {file} in the script must not be injectable. Only a short
// extension made of letters and digits is kept, so that tools which key on the
// extension, such as gofmt, still behave as on the real file.
func checkFileName(filePath string) string {
	ext := path.Ext(path.Base(filePath))
	if len(ext) < 2 || len(ext) > 16 {
		return "check"
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "check"
		}
	}
	return "check" + ext
}

// checkResolution is the built-in validation every answer must pass.
func checkResolution(res Resolution, maxBytes int64) error {
	switch res.Action {
	case protocol.ActionDelete:
		return nil
	case protocol.ActionWrite:
	default:
		return fmt.Errorf("unknown action %q", res.Action)
	}
	if !protocol.ValidMode(res.Mode) {
		return fmt.Errorf("invalid mode %q", res.Mode)
	}
	if int64(len(res.Content)) > maxBytes {
		return fmt.Errorf("content of %d bytes exceeds the %d byte limit", len(res.Content), maxBytes)
	}
	if !isBinary(res.Content) && HasConflictMarkers(res.Content) {
		return errors.New("content contains conflict markers")
	}
	return nil
}

// HasConflictMarkers reports whether content has a conflict marker line. It mirrors the
// server's acceptance rule
// (internal/graft/resolver/validate.go): a line is a conflict marker if it
// starts with "<<<<<<< " or ">>>>>>> ", or if it is "=======" followed by
// whitespace only. A trailing carriage return is ignored. A longer run of '='
// such as a Markdown setext or RST title underline is not a marker.
func HasConflictMarkers(content []byte) bool {
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		switch {
		case bytes.HasPrefix(line, []byte("<<<<<<< ")),
			bytes.HasPrefix(line, []byte(">>>>>>> ")):
			return true
		case bytes.HasPrefix(line, []byte("=======")) && len(bytes.TrimSpace(line[len("======="):])) == 0:
			return true
		}
	}
	return false
}

// isBinary uses the same heuristic as git for the whole content: a NUL byte.
func isBinary(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0
}

func validateRequest(req protocol.Request) error {
	if req.Version != protocol.Version {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidRequest, req.Version)
	}
	seen := make(map[string]bool, len(req.Files))
	for _, f := range req.Files {
		if !validPath(f.Path) {
			return fmt.Errorf("%w: unsafe path %q", ErrInvalidRequest, f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("%w: duplicate path %q", ErrInvalidRequest, f.Path)
		}
		seen[f.Path] = true
		if !protocol.ValidKind(f.Kind) {
			return fmt.Errorf("%w: unknown kind %q for %q", ErrInvalidRequest, f.Kind, f.Path)
		}
		sides := []struct {
			name string
			side *protocol.Side
		}{{"base", f.Base}, {"ours", f.Ours}, {"theirs", f.Theirs}}
		for _, s := range sides {
			if s.side != nil && !protocol.ValidMode(s.side.Mode) {
				return fmt.Errorf("%w: %s side of %q has invalid mode %q", ErrInvalidRequest, s.name, f.Path, s.side.Mode)
			}
		}
	}
	return nil
}

// validPath accepts relative, slash-separated, clean paths without ".." and
// without control characters or backslashes.
func validPath(p string) bool {
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return false
	}
	if strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return path.Clean(p) == p
}

// validPattern accepts a glob with "**" only as a whole pattern, as leading
// "**/" segments or as a trailing "/**" segment (see matchPath).
func validPattern(p string) error {
	if p == "" {
		return errors.New("empty path pattern")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("path pattern %q must be relative and use slashes", p)
	}
	if p == "**" {
		return nil
	}
	base := strings.TrimSuffix(p, "/**")
	if _, err := path.Match(base, ""); err != nil {
		return fmt.Errorf("invalid path pattern %q: %w", p, err)
	}
	return nil
}

func validatePatterns(patterns []string) error {
	for _, p := range patterns {
		if err := validPattern(p); err != nil {
			return err
		}
	}
	return nil
}

// matchPaths reports whether p matches any pattern; no patterns match all.
func matchPaths(patterns []string, p string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pat := range patterns {
		if matchPath(pat, p) {
			return true
		}
	}
	return false
}

// matchPath matches the full path against a glob. A trailing "/**" matches
// the directory itself and everything below it; "**" matches everything. A
// leading "**/" matches any number of leading directories, none included, so
// "**/*.go" matches "main.go" and "a/b/c.go".
func matchPath(pat, p string) bool {
	if pat == "**" {
		return true
	}
	if rest, ok := strings.CutPrefix(pat, "**/"); ok {
		for {
			if matchPath(rest, p) {
				return true
			}
			i := strings.IndexByte(p, '/')
			if i < 0 {
				return false
			}
			p = p[i+1:]
		}
	}
	if prefix, ok := strings.CutSuffix(pat, "/**"); ok {
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	ok, err := path.Match(pat, p)
	return err == nil && ok
}

// validName accepts the names used in the resolver string.
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// substitute replaces placeholders in every argument.
func substitute(argv []string, repl map[string]string) []string {
	pairs := make([]string, 0, 2*len(repl))
	for k, v := range repl {
		pairs = append(pairs, k, v)
	}
	r := strings.NewReplacer(pairs...)
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = r.Replace(a)
	}
	return out
}

func withStderr(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w (stderr: %s)", err, stderr)
}
