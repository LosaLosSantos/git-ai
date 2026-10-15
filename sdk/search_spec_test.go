package sdk

// Black-box spec tests for WP42 (search in the SDK). Written from
// doc/graft/PLAN5.md only. Not compiled against the implementation.
//
// ASSUMPTION: the *Repo construction is not named in the spec. specSearchRepo
// below assumes the same constructor other sdk tests use; adjust it if the
// sdk package exposes a different one.

import (
	"context"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
)

// specSearchRepo creates a repository whose default commit holds the given
// files and returns the repo handle and the commit id as a revision string.
func specSearchRepo(t *testing.T, files []gittest.TreeEntry) (*Repo, string) {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	cfg.SocketPath = addr

	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(specNestEntries(t, cfg, repoPath, files)...))

	repo := specNewRepo(t, ctx, addr, cfg.Storages[0].Name, repoProto.GetRelativePath())
	return repo, commit.String()
}

// specNestEntries turns entries whose Path contains slashes into nested trees,
// since gittest tree entries must be single path components.
func specNestEntries(t *testing.T, cfg config.Cfg, repoPath string, files []gittest.TreeEntry) []gittest.TreeEntry {
	t.Helper()
	var out []gittest.TreeEntry
	dirs := map[string][]gittest.TreeEntry{}
	var order []string
	for _, f := range files {
		dir, rest, ok := strings.Cut(f.Path, "/")
		if !ok {
			out = append(out, f)
			continue
		}
		if _, seen := dirs[dir]; !seen {
			order = append(order, dir)
		}
		f.Path = rest
		dirs[dir] = append(dirs[dir], f)
	}
	for _, dir := range order {
		oid := gittest.WriteTree(t, cfg, repoPath, specNestEntries(t, cfg, repoPath, dirs[dir]))
		out = append(out, gittest.TreeEntry{Mode: "040000", Path: dir, OID: oid})
	}
	return out
}

// specNewRepo dials the server with the sdk and returns the repository handle.
func specNewRepo(t *testing.T, ctx context.Context, addr, storage, relPath string) *Repo {
	t.Helper()
	client, err := Dial(ctx, addr, WithStorage(storage))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client.Repo(relPath)
}

func specSearchCtx(t *testing.T) context.Context {
	t.Helper()
	return testhelper.Context(t)
}

// TestSpecSearchCodeFixedStringByDefault checks that without Regex the query
// is literal: "a.c" must not match "abc".
func TestSpecSearchCodeFixedStringByDefault(t *testing.T) {
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "f.txt", Content: "abc\na.c\n"},
	})

	matches, _, _, err := repo.SearchCode(specSearchCtx(t), SearchOptions{Revision: rev, Query: "a.c"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(matches) != 1 || matches[0].Text != "a.c" || matches[0].Line != 2 {
		t.Fatalf("expected only the literal line 2 match, got %+v", matches)
	}
}

// TestSpecSearchCodeInvalidArguments checks InvalidArgument-class failures:
// an empty query, an invalid regex, and a query over 1 KiB must all error.
func TestSpecSearchCodeInvalidArguments(t *testing.T) {
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "f.txt", Content: "hello\n"},
	})
	ctx := specSearchCtx(t)

	if _, _, _, err := repo.SearchCode(ctx, SearchOptions{Revision: rev, Query: ""}); err == nil {
		t.Errorf("empty query: expected error")
	}
	if _, _, _, err := repo.SearchCode(ctx, SearchOptions{Revision: rev, Query: "(", Regex: true}); err == nil {
		t.Errorf("invalid regex: expected error")
	}
	big := strings.Repeat("x", 1025)
	if _, _, _, err := repo.SearchCode(ctx, SearchOptions{Revision: rev, Query: big}); err == nil {
		t.Errorf("query over 1 KiB: expected error")
	}
}

// TestSpecSearchCodeRegexAndCaseSensitivity checks that Regex enables regular
// expressions and that CaseSensitive:true distinguishes case, while
// CaseSensitive:false matches regardless of case.
func TestSpecSearchCodeRegexAndCaseSensitivity(t *testing.T) {
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "f.txt", Content: "Foo1\nfoo2\nbar3\n"},
	})
	ctx := specSearchCtx(t)

	sensitive, _, _, err := repo.SearchCode(ctx, SearchOptions{
		Revision: rev, Query: "^foo[0-9]$", Regex: true, CaseSensitive: true,
	})
	if err != nil {
		t.Fatalf("case-sensitive regex: %v", err)
	}
	if len(sensitive) != 1 || sensitive[0].Line != 2 {
		t.Fatalf("case-sensitive regex: expected only line 2, got %+v", sensitive)
	}

	insensitive, _, _, err := repo.SearchCode(ctx, SearchOptions{
		Revision: rev, Query: "^foo[0-9]$", Regex: true, CaseSensitive: false,
	})
	if err != nil {
		t.Fatalf("case-insensitive regex: %v", err)
	}
	if len(insensitive) != 2 {
		t.Fatalf("case-insensitive regex: expected lines 1 and 2, got %+v", insensitive)
	}
}

// TestSpecSearchCodeOrderingAndCommitID checks that matches are sorted by path
// then line, and that the returned commitID is the resolved revision.
func TestSpecSearchCodeOrderingAndCommitID(t *testing.T) {
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "b.txt", Content: "needle\nx\nneedle\n"},
		{Mode: "100644", Path: "a.txt", Content: "y\nneedle\n"},
	})

	matches, commitID, truncated, err := repo.SearchCode(specSearchCtx(t), SearchOptions{Revision: rev, Query: "needle"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if truncated {
		t.Errorf("unexpected truncated=true")
	}
	if commitID != rev {
		t.Errorf("commitID = %q, want %q", commitID, rev)
	}

	want := []struct {
		path string
		line int
	}{{"a.txt", 2}, {"b.txt", 1}, {"b.txt", 3}}
	if len(matches) != len(want) {
		t.Fatalf("got %d matches, want %d: %+v", len(matches), len(want), matches)
	}
	for i, w := range want {
		if matches[i].Path != w.path || matches[i].Line != w.line {
			t.Errorf("match %d = %s:%d, want %s:%d", i, matches[i].Path, matches[i].Line, w.path, w.line)
		}
	}
}

// TestSpecSearchCodeContextLinesCapped checks that Before/After carry the
// requested context and that the context is capped by the server limit
// (default 5) even when a larger ContextLines is requested.
func TestSpecSearchCodeContextLinesCapped(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		if i == 10 {
			sb.WriteString("TARGET\n")
		} else {
			sb.WriteString("line\n")
		}
	}
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "ctx.txt", Content: sb.String()},
	})
	ctx := specSearchCtx(t)

	matches, _, _, err := repo.SearchCode(ctx, SearchOptions{Revision: rev, Query: "TARGET", ContextLines: 2})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if len(matches[0].Before) != 2 || len(matches[0].After) != 2 {
		t.Errorf("ContextLines=2: Before=%d After=%d, want 2 and 2", len(matches[0].Before), len(matches[0].After))
	}

	matches, _, _, err = repo.SearchCode(ctx, SearchOptions{Revision: rev, Query: "TARGET", ContextLines: 50})
	if err != nil {
		t.Fatalf("SearchCode with large context: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if len(matches[0].Before) > 5 || len(matches[0].After) > 5 {
		t.Errorf("context not capped at 5: Before=%d After=%d", len(matches[0].Before), len(matches[0].After))
	}
}

// TestSpecSearchCodeMaxResultsTruncates checks that MaxResults stops the
// search and sets truncated=true with no error.
func TestSpecSearchCodeMaxResultsTruncates(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("hit\n")
	}
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "many.txt", Content: sb.String()},
	})

	matches, _, truncated, err := repo.SearchCode(specSearchCtx(t), SearchOptions{
		Revision: rev, Query: "hit", MaxResults: 3,
	})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(matches) != 3 {
		t.Errorf("got %d matches, want 3", len(matches))
	}
	if !truncated {
		t.Errorf("expected truncated=true when MaxResults is reached")
	}
}

// TestSpecSearchCodeNoMatchIsNotAnError checks that a query with no hits
// returns no matches, truncated=false and a nil error (git grep exit 1).
func TestSpecSearchCodeNoMatchIsNotAnError(t *testing.T) {
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "f.txt", Content: "nothing here\n"},
	})

	matches, _, truncated, err := repo.SearchCode(specSearchCtx(t), SearchOptions{Revision: rev, Query: "zzz-absent"})
	if err != nil {
		t.Fatalf("no match must not be an error, got %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no matches, got %+v", matches)
	}
	if truncated {
		t.Errorf("no match must not set truncated")
	}
}

// TestSpecSearchCodePathsFilter checks that Paths restricts the search to the
// given paths only.
func TestSpecSearchCodePathsFilter(t *testing.T) {
	repo, rev := specSearchRepo(t, []gittest.TreeEntry{
		{Mode: "100644", Path: "docs/guide.md", Content: "token\n"},
		{Mode: "100644", Path: "src/main.go", Content: "token\n"},
	})

	matches, _, _, err := repo.SearchCode(specSearchCtx(t), SearchOptions{
		Revision: rev, Query: "token", Paths: []string{"docs"},
	})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(matches) != 1 || matches[0].Path != "docs/guide.md" {
		t.Fatalf("expected only docs/guide.md, got %+v", matches)
	}
}
