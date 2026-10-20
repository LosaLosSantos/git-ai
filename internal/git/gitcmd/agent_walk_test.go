package gitcmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/identity"
)

func agentWalkContext() context.Context {
	return identity.NewContext(context.Background(), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
}

// TestAgentRefWalk: only an agent's commands that walk git's pseudo references are rewritten; a
// command whose --all means something else (merge-base) is not.
func TestAgentRefWalk(t *testing.T) {
	t.Parallel()

	agent := agentWalkContext()
	for _, name := range []string{"bundle", "fast-export", "format-patch", "log", "rev-list", "rev-parse", "show"} {
		require.True(t, agentRefWalk(agent, Command{Name: name}), name)
		require.False(t, agentRefWalk(context.Background(), Command{Name: name}), name)
	}
	for _, name := range []string{"merge-base", "cat-file", "for-each-ref", "unknown"} {
		require.False(t, agentRefWalk(agent, Command{Name: name}), name)
	}
	shared := identity.NewContext(context.Background(), identity.Identity{Name: "ci", Source: identity.SourceSharedToken})
	require.False(t, agentRefWalk(shared, Command{Name: "rev-list"}))
}

// TestExcludeHiddenRefs: the flag goes before each --all and --glob among the options and the
// arguments, and nowhere else: not before an option's value that reads --all, nor before
// --branches, which git refuses after it.
func TestExcludeHiddenRefs(t *testing.T) {
	t.Parallel()

	got := excludeHiddenRefs(Command{
		Name: "rev-list",
		Flags: []Option{
			Flag{Name: "--all"},
			ValueFlag{Name: "--grep", Value: "--all"},
			ValueFlag{Name: "--glob", Value: "refs/*"},
			Flag{Name: "--glob=refs/*"},
			Flag{Name: "--branches"},
		},
		Args:        []string{"main", "--not", "--all", "--glob=refs/heads/*", "--tags"},
		PostSepArgs: []string{"--all"},
	})
	require.Equal(t, []Option{
		Flag{Name: excludeHiddenFlag}, Flag{Name: "--all"},
		ValueFlag{Name: "--grep", Value: "--all"},
		Flag{Name: excludeHiddenFlag}, ValueFlag{Name: "--glob", Value: "refs/*"},
		Flag{Name: excludeHiddenFlag}, Flag{Name: "--glob=refs/*"},
		Flag{Name: "--branches"},
	}, got.Flags)
	require.Equal(t, []string{"main", "--not", excludeHiddenFlag, "--all", excludeHiddenFlag, "--glob=refs/heads/*", "--tags"}, got.Args)
	require.Equal(t, []string{"--all"}, got.PostSepArgs, "paths are left alone")

	args, err := got.CommandArgs()
	require.NoError(t, err, "the rewritten command passes gitcmd's own validation")
	require.Contains(t, args, excludeHiddenFlag)
}

// TestGitStdinLine: a line is what git reads: without its line feed, without the carriage return
// before it, and cut at the first NUL.
func TestGitStdinLine(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"--all":             "--all",
		"--all\n":           "--all",
		"--all\r\n":         "--all",
		"--all\r":           "--all",
		"--all\x00":         "--all",
		"--all\x00\n":       "--all",
		"--all\x00junk\r\n": "--all",
		"--all\r\r\n":       "--all\r",
		"--glob=x\x00y\n":   "--glob=x",
		"main\n":            "main",
		"":                  "",
	} {
		require.Equal(t, want, gitStdinLine(in), "%q", in)
	}
}

// TestExcludeHiddenRefsOnStdin_linesAsGitReadsThem: the lines "--all\r", "--all\x00" and the like are
// "--all" to git, so each gets the flag before it, and "--glob=" with them too.
func TestExcludeHiddenRefsOnStdin_linesAsGitReadsThem(t *testing.T) {
	t.Parallel()

	for _, line := range []string{"--all\r", "--all\x00", "--all\x00junk", "--glob=refs/*\r", "--glob=refs/*\x00x"} {
		out, err := io.ReadAll(excludeHiddenRefsOnStdin(strings.NewReader("main\n" + line + "\nmain2\n")))
		require.NoError(t, err)
		require.Equal(t, "main\n"+excludeHiddenFlag+"\n"+line+"\nmain2\n", string(out), "%q", line)
	}

	// After a line "--" with a carriage return too, the lines are paths.
	out, err := io.ReadAll(excludeHiddenRefsOnStdin(strings.NewReader("--\r\n--all\n")))
	require.NoError(t, err)
	require.Equal(t, "--\r\n--all\n", string(out))
}

// TestExcludeHiddenRefsOnStdin: on stdin, the flag goes on a line before each --all and --glob=,
// until the line "--" after which paths follow; the stream is otherwise unchanged, read in any
// chunks, and an error of the input reaches the reader.
func TestExcludeHiddenRefsOnStdin(t *testing.T) {
	t.Parallel()

	in := "main\n--all\n--not\n--glob=refs/*\n--branches\n--\n--all\nlast"
	want := "main\n" + excludeHiddenFlag + "\n--all\n--not\n" + excludeHiddenFlag + "\n--glob=refs/*\n--branches\n--\n--all\nlast"

	out, err := io.ReadAll(excludeHiddenRefsOnStdin(strings.NewReader(in)))
	require.NoError(t, err)
	require.Equal(t, want, string(out))

	out, err = io.ReadAll(iotest.OneByteReader(excludeHiddenRefsOnStdin(iotest.HalfReader(strings.NewReader(in)))))
	require.NoError(t, err)
	require.Equal(t, want, string(out))

	out, err = io.ReadAll(excludeHiddenRefsOnStdin(strings.NewReader("")))
	require.NoError(t, err)
	require.Empty(t, out)

	broken := errors.New("broken input")
	out, err = io.ReadAll(excludeHiddenRefsOnStdin(io.MultiReader(strings.NewReader("--all\n"), iotest.ErrReader(broken))))
	require.ErrorIs(t, err, broken)
	require.Equal(t, excludeHiddenFlag+"\n--all\n", string(out))
}
