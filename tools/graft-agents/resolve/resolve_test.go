package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// fakeStrategy answers with fn and counts its calls.
type fakeStrategy struct {
	fn    func(f File) (Resolution, bool, error)
	calls atomic.Int32
}

func (s *fakeStrategy) Resolve(_ context.Context, f File, _ protocol.Request) (Resolution, bool, error) {
	s.calls.Add(1)
	return s.fn(f)
}

// recordingStrategy also implements Recorder.
type recordingStrategy struct {
	fakeStrategy
	mu       sync.Mutex
	recorded []Resolution
	recErr   error
}

func (s *recordingStrategy) Record(_ context.Context, _ File, res Resolution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded = append(s.recorded, res)
	return s.recErr
}

func answer(mode, content string) func(File) (Resolution, bool, error) {
	return func(File) (Resolution, bool, error) {
		return Resolution{Action: protocol.ActionWrite, Mode: mode, Content: []byte(content)}, true, nil
	}
}

func decline(File) (Resolution, bool, error) { return Resolution{}, false, nil }

func failWith(msg string) func(File) (Resolution, bool, error) {
	return func(File) (Resolution, bool, error) { return Resolution{}, false, errors.New(msg) }
}

func sideOf(mode, content string) *protocol.Side {
	return &protocol.Side{Mode: mode, Content: []byte(content)}
}

func conflict(path string) protocol.FileConflict {
	return protocol.FileConflict{
		Path:   path,
		Kind:   protocol.KindContent,
		Base:   sideOf(protocol.ModeFile, "base\n"),
		Ours:   sideOf(protocol.ModeFile, "ours\n"),
		Theirs: sideOf(protocol.ModeFile, "theirs\n"),
	}
}

func request(files ...protocol.FileConflict) protocol.Request {
	return protocol.Request{
		Version: protocol.Version,
		Limits:  protocol.Limits{MaxFileBytes: 1 << 20},
		Files:   files,
	}
}

func newTestResolver(t *testing.T, checks []Check, bindings ...Binding) *Resolver {
	t.Helper()
	r, err := NewResolver(bindings, checks, time.Minute)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestResolveChainOrderAndNames(t *testing.T) {
	declines := &fakeStrategy{fn: decline}
	second := &fakeStrategy{fn: answer(protocol.ModeFile, "from second\n")}
	third := &fakeStrategy{fn: answer(protocol.ModeFile, "from third\n")}
	r := newTestResolver(t, nil,
		Binding{Name: "declines", Strategy: declines},
		Binding{Name: "second", Strategy: second},
		Binding{Name: "third", Strategy: third},
	)

	resp, err := r.Resolve(context.Background(), request(conflict("a.go")))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Resolver != "graft-resolve/second" {
		t.Fatalf("resolver = %q", resp.Resolver)
	}
	if string(resp.Files[0].Content) != "from second\n" || resp.Files[0].Action != protocol.ActionWrite ||
		resp.Files[0].Mode != protocol.ModeFile || resp.Files[0].Path != "a.go" {
		t.Fatalf("file = %+v", resp.Files[0])
	}
	if third.calls.Load() != 0 {
		t.Fatal("strategy after the accepting one was called")
	}
	if declines.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Fatalf("calls: declines=%d second=%d", declines.calls.Load(), second.calls.Load())
	}
	if !strings.Contains(resp.Explanation, "a.go=second") {
		t.Fatalf("explanation = %q", resp.Explanation)
	}
}

func TestResolveStrategyErrorContinuesChain(t *testing.T) {
	r := newTestResolver(t, nil,
		Binding{Name: "broken", Strategy: &fakeStrategy{fn: failWith("disk on fire")}},
		Binding{Name: "fallback", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "ok\n")}},
	)
	resp, err := r.Resolve(context.Background(), request(conflict("a.go")))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Resolver != "graft-resolve/fallback" {
		t.Fatalf("resolver = %q", resp.Resolver)
	}
}

func TestResolveUnresolvedReportsReasons(t *testing.T) {
	r := newTestResolver(t, nil,
		Binding{Name: "broken", Strategy: &fakeStrategy{fn: failWith("disk on fire")}},
		Binding{Name: "idle", Strategy: &fakeStrategy{fn: decline}},
	)
	_, err := r.Resolve(context.Background(), request(conflict("a.go"), conflict("b.go")))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error = %v, want *UnresolvedError", err)
	}
	if len(unresolved.Failures) != 2 {
		t.Fatalf("failures = %+v", unresolved.Failures)
	}
	if !strings.Contains(strings.Join(unresolved.Failures[0].Reasons, " "), "disk on fire") {
		t.Fatalf("reason lost: %+v", unresolved.Failures[0])
	}
	if !strings.Contains(err.Error(), "a.go") || !strings.Contains(err.Error(), "b.go") {
		t.Fatalf("error text = %q", err)
	}
}

func TestResolveAllOrNothing(t *testing.T) {
	// The first file resolves, the second does not: no partial answer.
	r := newTestResolver(t, nil, Binding{Name: "picky", Strategy: &fakeStrategy{fn: func(f File) (Resolution, bool, error) {
		if f.Path == "ok.go" {
			return Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("x")}, true, nil
		}
		return Resolution{}, false, nil
	}}})
	resp, err := r.Resolve(context.Background(), request(conflict("ok.go"), conflict("no.go")))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error = %v", err)
	}
	if len(resp.Files) != 0 {
		t.Fatalf("partial response returned: %+v", resp)
	}
	if len(unresolved.Failures) != 1 || unresolved.Failures[0].Path != "no.go" {
		t.Fatalf("failures = %+v", unresolved.Failures)
	}
}

func TestResolveRejectsInvalidAnswersAndContinues(t *testing.T) {
	cases := map[string]Resolution{
		"unknown action":       {Action: "rename"},
		"invalid mode":         {Action: protocol.ActionWrite, Mode: "777", Content: []byte("x")},
		"empty mode":           {Action: protocol.ActionWrite, Content: []byte("x")},
		"conflict markers":     {Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("a\n<<<<<<< ours\nb\n=======\nc\n>>>>>>> theirs\n")},
		"oversized content":    {Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte(strings.Repeat("x", 100))},
		"marker equals prefix": {Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("=======\n")},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			badStrategy := &fakeStrategy{fn: func(File) (Resolution, bool, error) { return bad, true, nil }}
			r := newTestResolver(t, nil,
				Binding{Name: "bad", Strategy: badStrategy},
				Binding{Name: "good", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "good\n")}},
			)
			req := request(conflict("a.go"))
			req.Limits.MaxFileBytes = 50
			resp, err := r.Resolve(context.Background(), req)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if resp.Resolver != "graft-resolve/good" || string(resp.Files[0].Content) != "good\n" {
				t.Fatalf("bad answer was not rejected: %+v", resp)
			}
		})
	}
}

func TestResolveRejectedAnswerLeavesNoResolutionWhenNothingElse(t *testing.T) {
	r := newTestResolver(t, nil, Binding{Name: "bad", Strategy: &fakeStrategy{fn: func(File) (Resolution, bool, error) {
		return Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("<<<<<<< HEAD\n")}, true, nil
	}}})
	_, err := r.Resolve(context.Background(), request(conflict("a.go")))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || !strings.Contains(err.Error(), "conflict markers") {
		t.Fatalf("error = %v", err)
	}
}

func TestBinaryContentMayContainMarkers(t *testing.T) {
	content := []byte("\x00binary\n<<<<<<< x\n")
	r := newTestResolver(t, nil, Binding{Name: "bin", Strategy: &fakeStrategy{fn: func(File) (Resolution, bool, error) {
		return Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: content}, true, nil
	}}})
	resp, err := r.Resolve(context.Background(), request(conflict("a.bin")))
	if err != nil {
		t.Fatalf("binary answer rejected: %v", err)
	}
	if string(resp.Files[0].Content) != string(content) {
		t.Fatal("binary content changed")
	}
}

func TestResolveDeleteAnswer(t *testing.T) {
	r := newTestResolver(t, nil, Binding{Name: "del", Strategy: &fakeStrategy{fn: func(File) (Resolution, bool, error) {
		return Resolution{Action: protocol.ActionDelete, Mode: protocol.ModeFile, Content: []byte("ignored")}, true, nil
	}}})
	resp, err := r.Resolve(context.Background(), request(conflict("gone.go")))
	if err != nil {
		t.Fatal(err)
	}
	f := resp.Files[0]
	if f.Action != protocol.ActionDelete || f.Mode != "" || f.Content != nil {
		t.Fatalf("delete answer = %+v", f)
	}
}

func TestResolveMultipleFilesOrderAndNames(t *testing.T) {
	r := newTestResolver(t, nil,
		Binding{Name: "md", Strategy: &fakeStrategy{fn: func(f File) (Resolution, bool, error) {
			if strings.HasSuffix(f.Path, ".md") {
				return Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("md")}, true, nil
			}
			return Resolution{}, false, nil
		}}, Paths: []string{"*.md"}},
		Binding{Name: "rest", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "rest")}},
	)
	resp, err := r.Resolve(context.Background(), request(conflict("a.go"), conflict("b.md"), conflict("c.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Resolver != "graft-resolve/rest,md" {
		t.Fatalf("resolver = %q", resp.Resolver)
	}
	got := []string{resp.Files[0].Path, resp.Files[1].Path, resp.Files[2].Path}
	if strings.Join(got, ",") != "a.go,b.md,c.txt" {
		t.Fatalf("response order = %v", got)
	}
	if string(resp.Files[1].Content) != "md" {
		t.Fatalf("b.md answered by the wrong strategy: %q", resp.Files[1].Content)
	}
}

func TestResolveLongStrategyNamesFitTheServerLimit(t *testing.T) {
	// Three valid 40-byte names would make a 136-byte resolver name, which the
	// server rejects, failing the whole resolution.
	names := []string{strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)}
	var bindings []Binding
	for i, name := range names {
		bindings = append(bindings, Binding{
			Name:     name,
			Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "x\n")},
			Paths:    []string{fmt.Sprintf("%d.go", i)},
		})
	}
	r := newTestResolver(t, nil, bindings...)

	resp, err := r.Resolve(context.Background(), request(conflict("0.go"), conflict("1.go"), conflict("2.go")))
	if err != nil {
		t.Fatal(err)
	}
	want := "graft-resolve/" + names[0] + "," + names[1] + ",+1"
	if resp.Resolver != want {
		t.Fatalf("resolver = %q, want %q", resp.Resolver, want)
	}
	if len(resp.Resolver) > protocol.MaxResolverNameBytes {
		t.Fatalf("resolver name is %d bytes", len(resp.Resolver))
	}
	for i, name := range names {
		if !strings.Contains(resp.Explanation, fmt.Sprintf("%d.go=%s", i, name)) {
			t.Fatalf("explanation lost %s: %q", name, resp.Explanation)
		}
	}
}

func TestResolverName(t *testing.T) {
	long := func(c string) string { return strings.Repeat(c, 64) }
	for _, tc := range []struct {
		used []string
		want string
	}{
		{used: nil, want: "graft-resolve/none"},
		{used: []string{"trivial", "union"}, want: "graft-resolve/trivial,union"},
		// Exactly at the limit (14 + 64 + 1 + 49 = 128 bytes): kept in full.
		{used: []string{long("a"), strings.Repeat("b", 49)}, want: "graft-resolve/" + long("a") + "," + strings.Repeat("b", 49)},
		{used: []string{long("a"), long("b")}, want: "graft-resolve/" + long("a") + ",+1"},
		{used: []string{long("a"), long("b"), long("c"), long("d")}, want: "graft-resolve/" + long("a") + ",+3"},
		{used: []string{"x", "y", long("a"), long("b")}, want: "graft-resolve/x,y," + long("a") + ",+1"},
	} {
		got := resolverName(tc.used)
		if got != tc.want {
			t.Errorf("resolverName(%q) = %q, want %q", tc.used, got, tc.want)
		}
		if len(got) > protocol.MaxResolverNameBytes {
			t.Errorf("resolverName(%q) is %d bytes", tc.used, len(got))
		}
	}

	// A first name that leaves no room for the count still yields a valid name.
	// Strategy names are at most 64 bytes, so this only guards the fallback.
	if got := resolverName([]string{strings.Repeat("z", 120), "y"}); got != "graft-resolve/+2" {
		t.Errorf("fallback name = %q", got)
	}
}

func TestResolveEmptyRequest(t *testing.T) {
	r := newTestResolver(t, nil, Binding{Name: "trivial", Strategy: trivial{}})
	resp, err := r.Resolve(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 0 || resp.Resolver != "graft-resolve/none" || resp.Version != protocol.Version {
		t.Fatalf("response = %+v", resp)
	}
}

func TestPathFilterSkipsStrategy(t *testing.T) {
	s := &fakeStrategy{fn: answer(protocol.ModeFile, "x")}
	r := newTestResolver(t, nil, Binding{Name: "docs", Strategy: s, Paths: []string{"docs/**"}})
	_, err := r.Resolve(context.Background(), request(conflict("src/a.go")))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error = %v", err)
	}
	if s.calls.Load() != 0 {
		t.Fatal("strategy called for a path outside its filter")
	}
	if _, err := r.Resolve(context.Background(), request(conflict("docs/a.md"))); err != nil {
		t.Fatalf("filtered-in path not resolved: %v", err)
	}
}

func TestRecorderSeesOnlyAcceptedAnswers(t *testing.T) {
	rec := &recordingStrategy{fakeStrategy: fakeStrategy{fn: answer(protocol.ModeFile, "accepted\n")}}
	bad := &fakeStrategy{fn: func(File) (Resolution, bool, error) {
		return Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("<<<<<<< x\n")}, true, nil
	}}
	r := newTestResolver(t, nil,
		Binding{Name: "bad", Strategy: bad},
		Binding{Name: "rec", Strategy: rec},
	)
	if _, err := r.Resolve(context.Background(), request(conflict("a.go"))); err != nil {
		t.Fatal(err)
	}
	if len(rec.recorded) != 1 || string(rec.recorded[0].Content) != "accepted\n" {
		t.Fatalf("recorded = %+v", rec.recorded)
	}
}

func TestRecordErrorIsLoggedNotFatal(t *testing.T) {
	rec := &recordingStrategy{fakeStrategy: fakeStrategy{fn: answer(protocol.ModeFile, "x")}, recErr: errors.New("read-only store")}
	r := newTestResolver(t, nil, Binding{Name: "rec", Strategy: rec})
	var logged []string
	r.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	if _, err := r.Resolve(context.Background(), request(conflict("a.go"))); err != nil {
		t.Fatalf("record failure failed the resolve: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "read-only store") {
		t.Fatalf("logged = %v", logged)
	}
}

func TestContextCanceled(t *testing.T) {
	r := newTestResolver(t, nil, Binding{Name: "a", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "x")}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Resolve(ctx, request(conflict("a.go")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestValidateRequest(t *testing.T) {
	good := conflict("a.go")
	cases := map[string]protocol.Request{
		"wrong version":     {Version: 2, Files: []protocol.FileConflict{good}},
		"absolute path":     {Version: 1, Files: []protocol.FileConflict{withPath(good, "/etc/passwd")}},
		"parent traversal":  {Version: 1, Files: []protocol.FileConflict{withPath(good, "../x")}},
		"inner traversal":   {Version: 1, Files: []protocol.FileConflict{withPath(good, "a/../../x")}},
		"empty path":        {Version: 1, Files: []protocol.FileConflict{withPath(good, "")}},
		"dot path":          {Version: 1, Files: []protocol.FileConflict{withPath(good, ".")}},
		"dotdot path":       {Version: 1, Files: []protocol.FileConflict{withPath(good, "..")}},
		"backslash":         {Version: 1, Files: []protocol.FileConflict{withPath(good, `a\b`)}},
		"control character": {Version: 1, Files: []protocol.FileConflict{withPath(good, "a\nb")}},
		"double slash":      {Version: 1, Files: []protocol.FileConflict{withPath(good, "a//b")}},
		"trailing slash":    {Version: 1, Files: []protocol.FileConflict{withPath(good, "a/")}},
		"NUL":               {Version: 1, Files: []protocol.FileConflict{withPath(good, "a\x00b")}},
		"duplicate path":    {Version: 1, Files: []protocol.FileConflict{good, good}},
		"unknown kind":      {Version: 1, Files: []protocol.FileConflict{withKind(good, "rename")}},
		"bad base mode":     {Version: 1, Files: []protocol.FileConflict{withBase(good, &protocol.Side{Mode: "000"})}},
	}
	r := newTestResolver(t, nil, Binding{Name: "a", Strategy: &fakeStrategy{fn: answer(protocol.ModeFile, "x")}})
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.Resolve(context.Background(), req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func withPath(f protocol.FileConflict, p string) protocol.FileConflict { f.Path = p; return f }
func withKind(f protocol.FileConflict, k string) protocol.FileConflict { f.Kind = k; return f }
func withBase(f protocol.FileConflict, s *protocol.Side) protocol.FileConflict {
	f.Base = s
	return f
}

func TestValidPath(t *testing.T) {
	for _, p := range []string{"a", "a/b.go", "deep/er/x.txt", ".hidden", "a..b", "...x"} {
		if !validPath(p) {
			t.Errorf("validPath(%q) = false", p)
		}
	}
	for _, p := range []string{"", ".", "..", "../a", "/a", "a/", "a//b", "a/./b", "a/../b", `a\b`, "a\x01", "a\x7f"} {
		if validPath(p) {
			t.Errorf("validPath(%q) = true", p)
		}
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**", "anything/at/all", true},
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docs", true},
		{"docs/**", "docsx/a", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"a/*.go", "a/x.go", true},
		{"a/*.go", "a/b/x.go", false},
		{"CHANGELOG.md", "CHANGELOG.md", true},
		{"CHANGELOG.md", "x/CHANGELOG.md", false},
		{"[", "x", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/x.go", true},
		{"**/*.go", "a/b/c/x.go", true},
		{"**/*.go", "a/x.md", false},
		{"**/CHANGELOG.md", "CHANGELOG.md", true},
		{"**/CHANGELOG.md", "pkg/a/CHANGELOG.md", true},
		{"**/CHANGELOG.md", "pkg/CHANGELOG.mdx", false},
		{"**/docs/**", "a/docs/b/c.md", true},
		{"**/docs/**", "a/docsx/b", false},
	}
	for _, c := range cases {
		if got := matchPath(c.pat, c.path); got != c.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
	if !matchPaths(nil, "any") {
		t.Error("empty patterns must match everything")
	}
}

func TestValidPattern(t *testing.T) {
	for _, p := range []string{"**", "a/**", "*.go", "a/b/c.txt", "a/*/c"} {
		if err := validPattern(p); err != nil {
			t.Errorf("validPattern(%q): %v", p, err)
		}
	}
	for _, p := range []string{"", "/abs", `a\b`, "a/[", "[/**"} {
		if err := validPattern(p); err == nil {
			t.Errorf("validPattern(%q) accepted", p)
		}
	}
}

func TestHasConflictMarkers(t *testing.T) {
	// The rule must match the server's (internal/graft/resolver/validate.go):
	// "=======" counts only when followed by whitespace, and a trailing CR is
	// ignored.
	for _, s := range []string{
		"<<<<<<< HEAD\n", "x\n=======\ny", ">>>>>>> branch\n",
		"=======\r\n", "x\n=======  \t\ny\n", "<<<<<<< HEAD\r\n", "=======",
	} {
		if !HasConflictMarkers([]byte(s)) {
			t.Errorf("markers not found in %q", s)
		}
	}
	for _, s := range []string{
		"<<<<<<<HEAD\n", "a=======b\n", " ======= indented\n", "x >>>>>>> y\n", "",
		"=======zz\n", "Changelog\n=========\n", "Title\n==========\r\nbody\n", "======== x\n",
	} {
		if HasConflictMarkers([]byte(s)) {
			t.Errorf("false positive in %q", s)
		}
	}
}

func TestResolveUnionKeepsSetextUnderline(t *testing.T) {
	// A Markdown setext heading or RST title underline is longer than the
	// "=======" separator and is accepted by the server, so the built-in check
	// must not reject an answer that keeps it.
	r := newTestResolver(t, nil, Binding{Name: "union", Strategy: union{}, Paths: []string{"CHANGELOG.md"}})
	resp, err := r.Resolve(context.Background(), request(protocol.FileConflict{
		Path:   "CHANGELOG.md",
		Kind:   protocol.KindContent,
		Base:   sideOf(protocol.ModeFile, "Changelog\n=========\n\nold\n"),
		Ours:   sideOf(protocol.ModeFile, "Changelog\n=========\n\nours\nold\n"),
		Theirs: sideOf(protocol.ModeFile, "Changelog\n=========\n\ntheirs\nold\n"),
	}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "Changelog\n=========\n\nours\ntheirs\nold\n"; string(resp.Files[0].Content) != want {
		t.Fatalf("content = %q, want %q", resp.Files[0].Content, want)
	}
}

func TestNewResolverValidation(t *testing.T) {
	ok := Binding{Name: "a", Strategy: trivial{}}
	if _, err := NewResolver(nil, nil, time.Minute); err == nil {
		t.Error("no strategies accepted")
	}
	if _, err := NewResolver([]Binding{ok}, nil, 0); err == nil {
		t.Error("zero timeout accepted")
	}
	if _, err := NewResolver([]Binding{{Name: "a"}}, nil, time.Minute); err == nil {
		t.Error("nil strategy accepted")
	}
	if _, err := NewResolver([]Binding{{Name: "bad name", Strategy: trivial{}}}, nil, time.Minute); err == nil {
		t.Error("invalid name accepted")
	}
	if _, err := NewResolver([]Binding{ok}, []Check{{Command: []string{"x"}}}, time.Minute); err == nil {
		t.Error("check without paths accepted")
	}
	if _, err := NewResolver([]Binding{ok}, []Check{{Paths: []string{"*"}}}, time.Minute); err == nil {
		t.Error("check without command accepted")
	}
	if _, err := NewResolver([]Binding{ok}, []Check{{Paths: []string{"*"}, Command: []string{"x"}, Timeout: -1}}, time.Minute); err == nil {
		t.Error("negative check timeout accepted")
	}
}

func TestConcurrentResolve(t *testing.T) {
	r := newTestResolver(t, nil, Binding{Name: "trivial", Strategy: trivial{}})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := conflict(fmt.Sprintf("f%d.go", i))
			f.Ours = sideOf(protocol.ModeFile, "same\n")
			f.Theirs = sideOf(protocol.ModeFile, "same\n")
			resp, err := r.Resolve(context.Background(), request(f))
			if err != nil {
				errs <- err
				return
			}
			if resp.Files[0].Path != fmt.Sprintf("f%d.go", i) || string(resp.Files[0].Content) != "same\n" {
				errs <- fmt.Errorf("wrong answer for %d: %+v", i, resp.Files[0])
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// proposingStrategy answers like fakeStrategy, as a model does: its answers are proposals.
type proposingStrategy struct{ fakeStrategy }

func (*proposingStrategy) proposes() {}

// The llm strategy's answers are proposals (PLAN10 14).
var _ proposer = (*llmStrategy)(nil)

// TestProposalsAreMarkedAndNotRecorded: a model's answer is marked as a proposal, which the server
// never lands, and no Recorder learns it, so that it can never come back as a rule's answer; a
// rule's answer is not marked, and is recorded.
func TestProposalsAreMarkedAndNotRecorded(t *testing.T) {
	model := &proposingStrategy{fakeStrategy{fn: answer(protocol.ModeFile, "proposed\n")}}
	rec := &recordingStrategy{fakeStrategy: fakeStrategy{fn: decline}}
	r := newTestResolver(t, nil, Binding{Name: "rec", Strategy: rec}, Binding{Name: "llm", Strategy: model})
	resp, err := r.Resolve(context.Background(), request(conflict("a.go")))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || !resp.Files[0].Proposal {
		t.Fatalf("files %+v, want one proposal", resp.Files)
	}
	if len(rec.recorded) != 0 {
		t.Fatalf("a proposal was recorded: %+v", rec.recorded)
	}

	rule := &recordingStrategy{fakeStrategy: fakeStrategy{fn: answer(protocol.ModeFile, "ruled\n")}}
	r = newTestResolver(t, nil, Binding{Name: "rule", Strategy: rule})
	resp, err = r.Resolve(context.Background(), request(conflict("a.go")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Files[0].Proposal || len(rule.recorded) != 1 {
		t.Fatalf("rule answer: proposal %v, recorded %d", resp.Files[0].Proposal, len(rule.recorded))
	}
}
