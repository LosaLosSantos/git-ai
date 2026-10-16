package localrepo

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// commitWriterFixtures are the trees and commits that random commit configurations refer to.
type commitWriterFixtures struct {
	trees   []git.ObjectID
	commits []git.ObjectID
}

func setupCommitWriterFixtures(t *testing.T, cfg config.Cfg, repoPath string) commitWriterFixtures {
	t.Helper()

	var fixtures commitWriterFixtures
	for i := 0; i < 3; i++ {
		fixtures.trees = append(fixtures.trees, gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
			{Path: fmt.Sprintf("file-%d", i), Mode: "100644", Content: fmt.Sprintf("content %d", i)},
		}))
	}

	for i := 0; i < 4; i++ {
		fixtures.commits = append(fixtures.commits, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithMessage(fmt.Sprintf("parent %d", i)),
		))
	}

	return fixtures
}

var (
	// Names and emails that git writes unchanged. Those that git changes are tested in
	// TestCommitWriter_fallbackInputs.
	commitWriterTestNames = []string{
		"Ada Lovelace",
		"Zoë Müller",
		"李 雷",
		"Ünïcödé 😀",
		"multi\tword",
		"Ömer \"Q\" Ünal",
		"O'Brien, Jr. X",
		"Józef",
		"Ελένη",
		"x",
	}

	commitWriterTestEmails = []string{
		"ada@example.com",
		"zoë@例え.jp",
		"user+tag@example.org",
		"a@b",
		"first.last@example.com",
	}

	commitWriterTestMessages = []string{
		"",
		"subject",
		"subject\n",
		"subject\n\nbody\n\n\n",
		"with trailers\n\ntrailers\n\nSigned-off-by: A <a@example.com>\nReviewed-by: B <b@example.com>",
		"trailing spaces   \n\n",
		"no trailing newline",
		"\n\nleading blank lines\n",
		"héllo 世界 🌍\n",
		"tab\tand \"quotes\" <angle> brackets\n",
	}
)

func pickRandom[T any](rng *rand.Rand, values []T) T {
	return values[rng.Intn(len(values))]
}

// randomCommitTime returns a time with nanoseconds and an offset between -12h and +14h in steps of
// 15 minutes.
func randomCommitTime(rng *rand.Rand) time.Time {
	offset := (rng.Intn(105) - 48) * 15 * 60
	return time.Unix(946684800+rng.Int63n(1_000_000_000), rng.Int63n(1_000_000_000)).In(time.FixedZone("", offset))
}

func randomWriteCommitConfig(rng *rand.Rand, fixtures commitWriterFixtures, i int) WriteCommitConfig {
	cfg := WriteCommitConfig{
		TreeID:         pickRandom(rng, fixtures.trees),
		AuthorName:     pickRandom(rng, commitWriterTestNames),
		AuthorEmail:    pickRandom(rng, commitWriterTestEmails),
		AuthorDate:     randomCommitTime(rng),
		CommitterName:  pickRandom(rng, commitWriterTestNames),
		CommitterEmail: pickRandom(rng, commitWriterTestEmails),
		CommitterDate:  randomCommitTime(rng),
		Message:        pickRandom(rng, commitWriterTestMessages),
		Sign:           rng.Intn(2) == 0,
	}

	if rng.Intn(3) == 0 {
		cfg.Message = fmt.Sprintf("generated %d\n\n%s\n", i, pickRandom(rng, commitWriterTestNames))
	}

	if rng.Intn(2) == 0 {
		cfg.GitConfig = config.Git{
			CommitterName:  "GitLab",
			CommitterEmail: "noreply@gitlab.com",
		}
	}

	// Duplicate parents are included on purpose: git commit-tree drops them and so must the writer.
	for n := rng.Intn(4); n > 0; n-- {
		cfg.Parents = append(cfg.Parents, pickRandom(rng, fixtures.commits))
	}

	return cfg
}

func newTestCommitWriter(t *testing.T, ctx context.Context, repo *Repo) *CommitWriter {
	t.Helper()

	writer, err := repo.NewCommitWriter(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })

	return writer
}

func TestCommitWriter_matchesWriteCommit(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterMatchesWriteCommit)
}

func testCommitWriterMatchesWriteCommit(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	for seed := 1; seed <= 50; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		writeCfg := randomWriteCommitConfig(rng, fixtures, seed)

		expectedOID, err := repo.WriteCommit(ctx, writeCfg)
		require.NoError(t, err, "seed %d", seed)

		oid, err := writer.Write(ctx, writeCfg)
		require.NoError(t, err, "seed %d", seed)
		require.Equal(t, expectedOID, oid, "seed %d", seed)

		commit, err := repo.ReadCommit(ctx, git.Revision(oid))
		require.NoError(t, err, "seed %d", seed)
		require.Equal(t, writeCfg.Message, string(commit.GetBody()), "seed %d", seed)
		require.Equal(t, writeCfg.TreeID.String(), commit.GetTreeId(), "seed %d", seed)
		require.Equal(t, writeCfg.AuthorDate.Unix(), commit.GetAuthor().GetDate().GetSeconds(), "seed %d", seed)
		require.Equal(t, writeCfg.AuthorDate.Format("-0700"), string(commit.GetAuthor().GetTimezone()), "seed %d", seed)
		require.Equal(t, writeCfg.CommitterDate.Unix(), commit.GetCommitter().GetDate().GetSeconds(), "seed %d", seed)
		require.Equal(t, writeCfg.CommitterDate.Format("-0700"), string(commit.GetCommitter().GetTimezone()), "seed %d", seed)

		var expectedParents []string
		seenParents := map[git.ObjectID]bool{}
		for _, parent := range writeCfg.Parents {
			if seenParents[parent] {
				continue
			}
			seenParents[parent] = true
			expectedParents = append(expectedParents, parent.String())
		}
		require.Equal(t, strings.Join(expectedParents, " "), strings.Join(commit.GetParentIds(), " "), "seed %d", seed)
	}

	// Temporary commit object files must be removed once their object IDs are read.
	tempDir, err := repo.StorageTempDir()
	require.NoError(t, err)
	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), "commit-object-")
	}
}

func TestCommitWriter_reference(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterReference)
}

func testCommitWriterReference(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	writeCfg := WriteCommitConfig{
		Reference:      "refs/heads/commit-writer",
		TreeID:         fixtures.trees[0],
		Parents:        fixtures.commits[:1],
		AuthorName:     "Ada",
		AuthorEmail:    "ada@example.com",
		AuthorDate:     time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
		CommitterName:  "Ada",
		CommitterEmail: "ada@example.com",
		CommitterDate:  time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
		Message:        "via writer\n",
	}

	oid, err := writer.Write(ctx, writeCfg)
	require.NoError(t, err)

	ref := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "refs/heads/commit-writer")))
	require.Equal(t, oid.String(), ref)
}

func TestCommitWriter_unsupported(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterUnsupported)
}

func testCommitWriterUnsupported(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func() WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			Message:        "message\n",
		}
	}

	t.Run("alternate object directory", func(t *testing.T) {
		writeCfg := base()
		writeCfg.AlternateObjectDir = filepath.Join(t.TempDir(), "alternate")

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("signing", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Sign = true
		writeCfg.GitConfig = config.Git{SigningKey: "some-key"}

		_, err := writer.Write(ctx, writeCfg)
		if featureflag.GPGSigning.IsEnabled(ctx) {
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)
		} else {
			require.NoError(t, err)
		}
	})

	t.Run("invalid UTF-8 message", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Message = "broken \xff\xfe message\n"

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("invalid UTF-8 name", func(t *testing.T) {
		writeCfg := base()
		writeCfg.AuthorName = "Ada \xff"

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("NUL byte in message", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Message = "before\x00after\n"

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("name consisting only of disallowed characters", func(t *testing.T) {
		writeCfg := base()
		writeCfg.AuthorName = "<<>>"

		// The fallback to WriteCommit reports the error exactly as WriteCommit does.
		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Equal(t, ErrDisallowedCharacters, err)
	})

	t.Run("missing tree", func(t *testing.T) {
		writeCfg := base()
		writeCfg.TreeID = ""

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrMissingTree)
	})

	t.Run("missing author name", func(t *testing.T) {
		writeCfg := base()
		writeCfg.AuthorName = ""

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrMissingAuthorName)
	})
}

func TestCommitWriter_unsupportedTimes(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterUnsupportedTimes)
}

func testCommitWriterUnsupportedTimes(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func(when time.Time) WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     when,
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  when,
			Message:        "message\n",
		}
	}

	t.Run("before the Unix epoch", func(t *testing.T) {
		writeCfg := base(time.Unix(-31536000, 0).UTC())

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Error(t, err)

		// The rejected write must not reach hash-object, so later writes still work.
		valid := base(time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC))
		want, err := repo.WriteCommit(ctx, valid)
		require.NoError(t, err)

		got, err := writer.Write(ctx, valid)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("offset that is not a whole number of minutes", func(t *testing.T) {
		when := time.Unix(1700000000, 0).In(time.FixedZone("", 1172))

		_, err := writer.Write(ctx, base(when))
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("negative offset that is not a whole number of minutes", func(t *testing.T) {
		when := time.Unix(1700000000, 0).In(time.FixedZone("", -(3*3600 + 59)))

		_, err := writer.Write(ctx, base(when))
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("year after 2099", func(t *testing.T) {
		writeCfg := base(time.Date(2150, time.January, 1, 0, 0, 0, 0, time.UTC))

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Error(t, err)
	})
}

func TestCommitWriter_missingOrWrongTypeObjects(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterMissingOrWrongTypeObjects)
}

func testCommitWriterMissingOrWrongTypeObjects(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func() WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			Parents:        fixtures.commits[:1],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			Message:        "message\n",
		}
	}

	missing := gittest.DefaultObjectHash.ZeroOID

	t.Run("missing tree", func(t *testing.T) {
		writeCfg := base()
		writeCfg.TreeID = missing

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Error(t, err)
	})

	t.Run("missing parent", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Parents = []git.ObjectID{missing}

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Error(t, err)
	})

	t.Run("commit given as tree", func(t *testing.T) {
		writeCfg := base()
		writeCfg.TreeID = fixtures.commits[0]

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("tree given as parent", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Parents = []git.ObjectID{fixtures.trees[0]}

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})
}

func TestCommitWriter_commitEncoding(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterCommitEncoding)
}

func testCommitWriterCommitEncoding(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)

	writeCfg := WriteCommitConfig{
		TreeID:         fixtures.trees[0],
		Parents:        fixtures.commits[:1],
		AuthorName:     "Ada",
		AuthorEmail:    "ada@example.com",
		AuthorDate:     time.Unix(1700000000, 0).UTC(),
		CommitterName:  "Ada",
		CommitterEmail: "ada@example.com",
		CommitterDate:  time.Unix(1700000000, 0).UTC(),
		Message:        "msg\n",
	}

	t.Run("non-UTF-8 encoding is unsupported", func(t *testing.T) {
		gittest.Exec(t, cfg, "-C", repoPath, "config", "i18n.commitEncoding", "ISO-8859-1")
		writer := newTestCommitWriter(t, ctx, repo)

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		// WriteCommit writes the encoding header, so the fallback is what produces the right object.
		oid, err := repo.WriteCommit(ctx, writeCfg)
		require.NoError(t, err)
		require.Contains(t, string(gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-p", oid.String())), "encoding ISO-8859-1")
	})

	t.Run("UTF-8 encoding matches WriteCommit", func(t *testing.T) {
		gittest.Exec(t, cfg, "-C", repoPath, "config", "i18n.commitEncoding", "UTF-8")
		writer := newTestCommitWriter(t, ctx, repo)

		want, err := repo.WriteCommit(ctx, writeCfg)
		require.NoError(t, err)

		got, err := writer.Write(ctx, writeCfg)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
}

func TestCommitWriter_restartsFailedProcess(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterRestartsFailedProcess)
}

func testCommitWriterRestartsFailedProcess(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)

	writeCfg := func(i int) WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			Parents:        fixtures.commits[:1],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     time.Unix(1700000000+int64(i), 0).UTC(),
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  time.Unix(1700000000+int64(i), 0).UTC(),
			Message:        fmt.Sprintf("message %d\n", i),
		}
	}

	requireWriteMatches := func(t *testing.T, writer *CommitWriter, i int) {
		t.Helper()

		want, err := repo.WriteCommit(ctx, writeCfg(i))
		require.NoError(t, err)

		got, err := writer.Write(ctx, writeCfg(i))
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	t.Run("killed process", func(t *testing.T) {
		writer := newTestCommitWriter(t, ctx, repo)
		requireWriteMatches(t, writer, 1)

		oldPid := writer.process.cmd.Pid()
		require.NoError(t, syscall.Kill(oldPid, syscall.SIGKILL))

		// Every later Write succeeds on a new process with the same object ID as WriteCommit.
		for i := 2; i <= 4; i++ {
			requireWriteMatches(t, writer, i)
		}
		require.NotEqual(t, oldPid, writer.process.cmd.Pid())
	})

	t.Run("killed and reaped process", func(t *testing.T) {
		writer := newTestCommitWriter(t, ctx, repo)
		requireWriteMatches(t, writer, 5)

		require.NoError(t, syscall.Kill(writer.process.cmd.Pid(), syscall.SIGKILL))
		require.Error(t, writer.process.cmd.Wait())

		requireWriteMatches(t, writer, 6)
		requireWriteMatches(t, writer, 7)
	})

	t.Run("closed stdin", func(t *testing.T) {
		writer := newTestCommitWriter(t, ctx, repo)

		require.NoError(t, writer.process.cmd.Stdin().Close())

		requireWriteMatches(t, writer, 8)
		requireWriteMatches(t, writer, 9)
	})

	t.Run("canceled Write context", func(t *testing.T) {
		writer := newTestCommitWriter(t, ctx, repo)

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()

		// Like WriteCommit, a Write with a canceled context fails, and it writes nothing.
		_, err := writer.Write(canceledCtx, writeCfg(10))
		require.ErrorIs(t, err, context.Canceled)

		_, err = repo.WriteCommit(canceledCtx, writeCfg(10))
		require.Error(t, err)

		requireWriteMatches(t, writer, 11)
	})

	t.Run("canceled writer context", func(t *testing.T) {
		writerCtx, cancel := context.WithCancel(ctx)
		writer, err := repo.NewCommitWriter(writerCtx)
		require.NoError(t, err)

		cancel()

		// The processes of the writer end with its context, so the writer cannot be restarted.
		for i := 0; i < 2; i++ {
			_, err = writer.Write(ctx, writeCfg(12))
			require.ErrorIs(t, err, context.Canceled)
		}

		_ = writer.Close()
	})

	t.Run("Write after Close", func(t *testing.T) {
		writer, err := repo.NewCommitWriter(ctx)
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		_, err = writer.Write(ctx, writeCfg(13))
		require.Equal(t, errCommitWriterClosed, err)
		require.Nil(t, writer.process)
	})
}

func TestCommitWriter_fallbackInputs(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterFallbackInputs)
}

func testCommitWriterFallbackInputs(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func() WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			Parents:        fixtures.commits[:1],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			Message:        "message\n",
		}
	}

	// git changes or rejects these inputs, or did so in some version, so the writer must leave them
	// to WriteCommit.
	for _, tc := range []struct {
		desc   string
		modify func(*WriteCommitConfig)
	}{
		{desc: "CRLF message", modify: func(c *WriteCommitConfig) { c.Message = "carriage\r\nreturns\r\n" }},
		{desc: "CR-only message", modify: func(c *WriteCommitConfig) { c.Message = "a\rb\r" }},
		{desc: "CR inside the author name", modify: func(c *WriteCommitConfig) { c.AuthorName = "A\rda" }},
		{desc: "CR at the end of the committer name", modify: func(c *WriteCommitConfig) { c.CommitterName = "Ada\r" }},
		{desc: "CR in the author email", modify: func(c *WriteCommitConfig) { c.AuthorEmail = "ada\r@example.com" }},
		{desc: "padded name", modify: func(c *WriteCommitConfig) { c.AuthorName = "  padded  " }},
		{desc: "quoted name", modify: func(c *WriteCommitConfig) { c.AuthorName = "'quoted'" }},
		{desc: "name starting with a dot", modify: func(c *WriteCommitConfig) { c.AuthorName = ".dots" }},
		{desc: "name ending with a dot", modify: func(c *WriteCommitConfig) { c.CommitterName = "Jr." }},
		{desc: "name with angle brackets", modify: func(c *WriteCommitConfig) { c.AuthorName = "a<b>c" }},
		{desc: "name with a newline", modify: func(c *WriteCommitConfig) { c.CommitterName = "A\nB" }},
		{desc: "name ending with a tab", modify: func(c *WriteCommitConfig) { c.AuthorName = "Ada\t" }},
		{desc: "name ending with a backslash", modify: func(c *WriteCommitConfig) { c.AuthorName = "Ada\\" }},
		{desc: "name starting with a comma", modify: func(c *WriteCommitConfig) { c.CommitterName = ",Ada" }},
		{desc: "name of crud only", modify: func(c *WriteCommitConfig) { c.AuthorName = " ;:. " }},
		{desc: "padded email", modify: func(c *WriteCommitConfig) { c.AuthorEmail = "  spaced@example.com  " }},
		{desc: "email in angle brackets", modify: func(c *WriteCommitConfig) { c.AuthorEmail = "<weird>@example.com" }},
		{desc: "email ending with a dot", modify: func(c *WriteCommitConfig) { c.CommitterEmail = "ada@example.com." }},
		{desc: "empty author email", modify: func(c *WriteCommitConfig) { c.AuthorEmail = "" }},
		{desc: "empty committer email", modify: func(c *WriteCommitConfig) { c.CommitterEmail = "" }},
		{desc: "NUL in the committer email", modify: func(c *WriteCommitConfig) { c.CommitterEmail = "a\x00@example.com" }},
		{desc: "invalid UTF-8 email", modify: func(c *WriteCommitConfig) { c.AuthorEmail = "ada\xc3@example.com" }},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			writeCfg := base()
			tc.modify(&writeCfg)

			objectsBefore := gittest.Exec(t, cfg, "-C", repoPath, "count-objects")

			_, err := writer.Write(ctx, writeCfg)
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)

			// The fallback writes nothing.
			require.Equal(t, objectsBefore, gittest.Exec(t, cfg, "-C", repoPath, "count-objects"))
		})
	}

	// The writer stays usable after all those fallbacks.
	want, err := repo.WriteCommit(ctx, base())
	require.NoError(t, err)
	got, err := writer.Write(ctx, base())
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestCommitWriter_timezoneOffsets(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterTimezoneOffsets)
}

func testCommitWriterTimezoneOffsets(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func(authorDate, committerDate time.Time) WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     authorDate,
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  committerDate,
			Message:        "message\n",
		}
	}

	when := time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC)
	valid := when.In(time.FixedZone("", 3600))

	// git ignores offsets of 24 hours or more and uses the local timezone instead.
	for _, offset := range []time.Duration{
		24 * time.Hour,
		-24 * time.Hour,
		-25 * time.Hour,
		24*time.Hour + 30*time.Minute,
		99 * time.Hour,
		-99 * time.Hour,
	} {
		zoned := when.In(time.FixedZone("", int(offset.Seconds())))

		t.Run(fmt.Sprintf("author offset %s", offset), func(t *testing.T) {
			_, err := writer.Write(ctx, base(zoned, valid))
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)
		})

		t.Run(fmt.Sprintf("committer offset %s", offset), func(t *testing.T) {
			_, err := writer.Write(ctx, base(valid, zoned))
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)
		})
	}

	for _, offset := range []time.Duration{
		23*time.Hour + 59*time.Minute,
		-(23*time.Hour + 59*time.Minute),
		18 * time.Hour,
		-18 * time.Hour,
		0,
	} {
		t.Run(fmt.Sprintf("offset %s matches WriteCommit", offset), func(t *testing.T) {
			zoned := when.In(time.FixedZone("", int(offset.Seconds())))
			writeCfg := base(zoned, zoned)

			want, err := repo.WriteCommit(ctx, writeCfg)
			require.NoError(t, err)

			got, err := writer.Write(ctx, writeCfg)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestCommitWriter_utf8Noncharacters(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterUTF8Noncharacters)
}

func testCommitWriterUTF8Noncharacters(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func() WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			Message:        "message\n",
		}
	}

	// git re-encodes the bytes of these noncharacters, so the writer must leave them to WriteCommit.
	noncharacters := []rune{0xFFFE, 0xFFFF, 0x1FFFE, 0x10FFFF, 0xFDD0, 0xFDEF}

	for _, noncharacter := range noncharacters {
		t.Run(fmt.Sprintf("message containing U+%04X", noncharacter), func(t *testing.T) {
			writeCfg := base()
			writeCfg.Message = "x " + string(noncharacter) + " y\n"

			_, err := writer.Write(ctx, writeCfg)
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)
		})

		t.Run(fmt.Sprintf("author name containing U+%04X", noncharacter), func(t *testing.T) {
			writeCfg := base()
			writeCfg.AuthorName = "A " + string(noncharacter)

			_, err := writer.Write(ctx, writeCfg)
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)
		})

		t.Run(fmt.Sprintf("committer name containing U+%04X", noncharacter), func(t *testing.T) {
			writeCfg := base()
			writeCfg.CommitterName = "C " + string(noncharacter)

			_, err := writer.Write(ctx, writeCfg)
			require.ErrorIs(t, err, ErrCommitWriterUnsupported)
		})
	}

	t.Run("characters next to the noncharacters are written like WriteCommit", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Message = "x � ﷏ ﷰ \U0001FFFD ￼ y\n"

		want, err := repo.WriteCommit(ctx, writeCfg)
		require.NoError(t, err)

		got, err := writer.Write(ctx, writeCfg)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
}

func TestCommitWriter_isUTF8CommitEncoding(t *testing.T) {
	t.Parallel()

	// The names are classified as git's same_utf_encoding does: "utf" in any case, at most one '-',
	// then "8".
	for name, want := range map[string]bool{
		"utf8":       true,
		"UTF8":       true,
		"utf-8":      true,
		"UTF-8":      true,
		"Utf-8":      true,
		"UTF_8":      false,
		"utf--8":     false,
		"utf-08":     false,
		"utf-8-":     false,
		"utf":        false,
		"8":          false,
		"iso-8859-1": false,
	} {
		require.Equal(t, want, isUTF8CommitEncoding(name), "encoding %q", name)
	}
}

func TestCommitWriter_nonCanonicalUTF8Encoding(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterNonCanonicalUTF8Encoding)
}

func testCommitWriterNonCanonicalUTF8Encoding(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)

	gittest.Exec(t, cfg, "-C", repoPath, "config", "i18n.commitEncoding", "UTF_8")
	writer := newTestCommitWriter(t, ctx, repo)

	writeCfg := WriteCommitConfig{
		TreeID:         fixtures.trees[0],
		Parents:        fixtures.commits[:1],
		AuthorName:     "Ada",
		AuthorEmail:    "ada@example.com",
		AuthorDate:     time.Unix(1700000000, 0).UTC(),
		CommitterName:  "Ada",
		CommitterEmail: "ada@example.com",
		CommitterDate:  time.Unix(1700000000, 0).UTC(),
		Message:        "msg\n",
	}

	// git writes an encoding header for UTF_8 because it is not spelled as git recognises UTF-8, so
	// the writer must not write the object without it.
	_, err := writer.Write(ctx, writeCfg)
	require.ErrorIs(t, err, ErrCommitWriterUnsupported)

	oid, err := repo.WriteCommit(ctx, writeCfg)
	require.NoError(t, err)
	require.Contains(t, string(gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-p", oid.String())), "encoding UTF_8")
}

func TestCommitWriter_localYearBounds(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterLocalYearBounds)
}

func testCommitWriterLocalYearBounds(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func(when time.Time) WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     when,
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  when,
			Message:        "message\n",
		}
	}

	t.Run("local year before 1970 while the UTC year is 1970", func(t *testing.T) {
		when := time.Date(1969, time.December, 31, 20, 0, 0, 0, time.FixedZone("", -5*3600))
		require.Equal(t, 1970, when.UTC().Year())

		writeCfg := base(when)
		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Error(t, err)
	})

	t.Run("local year after 2099 while the UTC year is 2099", func(t *testing.T) {
		when := time.Date(2100, time.January, 1, 5, 0, 0, 0, time.FixedZone("", 6*3600))
		require.Equal(t, 2099, when.UTC().Year())

		writeCfg := base(when)
		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)

		_, err = repo.WriteCommit(ctx, writeCfg)
		require.Error(t, err)
	})
}

func TestCommitWriter_unsupportedIDFormats(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterUnsupportedIDFormats)
}

func testCommitWriterUnsupportedIDFormats(t *testing.T, ctx context.Context) {
	cfg, repo, repoPath := setupRepo(t)
	fixtures := setupCommitWriterFixtures(t, cfg, repoPath)
	writer := newTestCommitWriter(t, ctx, repo)

	base := func() WriteCommitConfig {
		return WriteCommitConfig{
			TreeID:         fixtures.trees[0],
			Parents:        fixtures.commits[:1],
			AuthorName:     "Ada",
			AuthorEmail:    "ada@example.com",
			AuthorDate:     time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			CommitterName:  "Ada",
			CommitterEmail: "ada@example.com",
			CommitterDate:  time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC),
			Message:        "message\n",
		}
	}

	t.Run("uppercase tree ID", func(t *testing.T) {
		writeCfg := base()
		writeCfg.TreeID = git.ObjectID(strings.ToUpper(fixtures.trees[0].String()))

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("uppercase parent ID", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Parents = []git.ObjectID{git.ObjectID(strings.ToUpper(fixtures.commits[0].String()))}

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})

	t.Run("abbreviated parent ID", func(t *testing.T) {
		writeCfg := base()
		writeCfg.Parents = []git.ObjectID{git.ObjectID(fixtures.commits[0].String()[:10])}

		_, err := writer.Write(ctx, writeCfg)
		require.ErrorIs(t, err, ErrCommitWriterUnsupported)
	})
}

func TestCommitWriter_closeIsIdempotent(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testCommitWriterCloseIsIdempotent)
}

func testCommitWriterCloseIsIdempotent(t *testing.T, ctx context.Context) {
	_, repo, _ := setupRepo(t)

	t.Run("repeated Close", func(t *testing.T) {
		writer, err := repo.NewCommitWriter(ctx)
		require.NoError(t, err)

		require.NoError(t, writer.Close())
		require.NoError(t, writer.Close())
	})

	t.Run("Close after context cancellation", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		writer, err := repo.NewCommitWriter(cancelCtx)
		require.NoError(t, err)

		cancel()

		// The process is reaped by the cancellation, so stdin is already closed. That must not be
		// reported as a failure of Close, and later calls must return the same result.
		first := writer.Close()
		require.NotErrorIs(t, first, os.ErrClosed)
		require.Equal(t, first, writer.Close())
	})
}
