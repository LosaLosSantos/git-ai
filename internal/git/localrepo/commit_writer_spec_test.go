package localrepo

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// specFixtures creates a repository with a chain of 4 commits (each the parent of
// the next, so all IDs are distinct) and returns the Repo, the empty tree and the
// commit IDs to use as parents.
func specFixtures(t *testing.T) (repo *Repo, tree git.ObjectID, parents []git.ObjectID) {
	t.Helper()
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	prev := gittest.WriteCommit(t, cfg, repoPath)
	parents = append(parents, prev)
	for i := 0; i < 3; i++ {
		prev = gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(prev))
		parents = append(parents, prev)
	}

	repo = NewTestRepo(t, cfg, repoProto)
	tree = gittest.DefaultObjectHash.EmptyTreeOID
	return repo, tree, parents
}

func specEdgeConfigs(tree git.ObjectID, parents []git.ObjectID) []WriteCommitConfig {
	tz := time.FixedZone("", -7*3600)
	tzPlus := time.FixedZone("", 5*3600+30*60)
	when := time.Unix(1700000000, 0)

	return []WriteCommitConfig{
		{
			TreeID:         tree,
			AuthorName:     "Zoë Ünïcode 名前",
			AuthorEmail:    "zoe@example.com",
			AuthorDate:     when.In(tz),
			CommitterName:  "Committer",
			CommitterEmail: "committer@example.com",
			CommitterDate:  when.In(tz),
			Message:        "subject\n\nbody\n",
		},
		{
			TreeID:         tree,
			Parents:        parents[:3],
			AuthorName:     "Multi Parent",
			AuthorEmail:    "mp@example.com",
			AuthorDate:     when.In(tzPlus),
			CommitterName:  "Multi Parent",
			CommitterEmail: "mp@example.com",
			CommitterDate:  when.In(tzPlus),
			Message:        "merge\n",
		},
		{
			TreeID:         tree,
			AuthorName:     "Empty",
			AuthorEmail:    "empty@example.com",
			AuthorDate:     when.In(time.UTC),
			CommitterName:  "Empty",
			CommitterEmail: "empty@example.com",
			CommitterDate:  when.In(time.UTC),
			Message:        "",
		},
		{
			TreeID:         tree,
			Parents:        parents[:1],
			AuthorName:     "No Newline",
			AuthorEmail:    "nn@example.com",
			AuthorDate:     when.In(tz),
			CommitterName:  "No Newline",
			CommitterEmail: "nn@example.com",
			CommitterDate:  when.In(tz),
			Message:        "subject without trailing newline",
		},
		{
			TreeID:         tree,
			Parents:        parents[:2],
			AuthorName:     "Trailers",
			AuthorEmail:    "tr@example.com",
			AuthorDate:     when.In(tzPlus),
			CommitterName:  "Trailers",
			CommitterEmail: "tr@example.com",
			CommitterDate:  when.In(tzPlus),
			Message:        "Subject\n\nSigned-off-by: A <a@example.com>\nCo-authored-by: B <b@example.com>\n",
		},
	}
}

// TestSpecWriteMatchesWriteCommit checks the explicit edge cases from the spec:
// unicode names, timezones, multiple parents, empty message, message without
// trailing newline and trailers must produce the same OIDs as WriteCommit.
func TestSpecWriteMatchesWriteCommit(t *testing.T) {
	repo, tree, parents := specFixtures(t)
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	w, err := repo.NewCommitWriter(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()

	for i, cfg := range specEdgeConfigs(tree, parents) {
		want, err := repo.WriteCommit(ctx, cfg)
		require.NoError(t, err, "case %d", i)

		got, err := w.Write(ctx, cfg)
		require.NoError(t, err, "case %d", i)
		require.Equal(t, want, got, "case %d: OID mismatch", i)
	}
}

// TestSpecWriteRandomConfigsMatchWriteCommit runs 50 random configs through one
// long-lived writer and requires identical OIDs to WriteCommit.
func TestSpecWriteRandomConfigsMatchWriteCommit(t *testing.T) {
	repo, tree, parents := specFixtures(t)
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	names := []string{"Ada", "Zoë Ünïcode", "名前 太郎", "Ørjan", "x", "Émile Zola"}
	messages := []string{"", "a", "subject\n", "subject\n\nbody line\n\nTrailer: v\n", "no newline", "多言語\n\n本文"}
	zones := []*time.Location{
		time.UTC,
		time.FixedZone("", -12*3600),
		time.FixedZone("", 14*3600),
		time.FixedZone("", -(3*3600 + 30*60)),
		time.FixedZone("", 5*3600+45*60),
	}

	rng := rand.New(rand.NewSource(1))

	w, err := repo.NewCommitWriter(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()

	for i := 0; i < 50; i++ {
		nParents := rng.Intn(len(parents) + 1)
		cfg := WriteCommitConfig{
			TreeID:         tree,
			Parents:        parents[:nParents],
			AuthorName:     names[rng.Intn(len(names))],
			AuthorEmail:    fmt.Sprintf("user%d@example.com", rng.Intn(100)),
			AuthorDate:     time.Unix(int64(rng.Intn(2000000000)), 0).In(zones[rng.Intn(len(zones))]),
			CommitterName:  names[rng.Intn(len(names))],
			CommitterEmail: fmt.Sprintf("c%d@example.com", rng.Intn(100)),
			CommitterDate:  time.Unix(int64(rng.Intn(2000000000)), 0).In(zones[rng.Intn(len(zones))]),
			Message:        messages[rng.Intn(len(messages))],
		}

		want, err := repo.WriteCommit(ctx, cfg)
		require.NoError(t, err, "random case %d", i)

		got, err := w.Write(ctx, cfg)
		require.NoError(t, err, "random case %d", i)
		require.Equal(t, want, got, "random case %d: OID mismatch", i)
	}
}

// TestSpecWriteObjectsReadableViaReadCommit checks that objects written by the
// streaming writer are real commits that ReadCommit can read back.
func TestSpecWriteObjectsReadableViaReadCommit(t *testing.T) {
	repo, tree, parents := specFixtures(t)
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	w, err := repo.NewCommitWriter(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()

	for i, cfg := range specEdgeConfigs(tree, parents) {
		oid, err := w.Write(ctx, cfg)
		require.NoError(t, err, "case %d", i)

		commit, err := repo.ReadCommit(ctx, oid.Revision())
		require.NoError(t, err, "case %d", i)
		require.Equal(t, oid.String(), commit.GetId(), "case %d", i)
	}
}

// TestSpecWriteReusesWriterAcrossManyWrites checks that a single writer stays
// correct over many consecutive writes (one long-lived hash-object process).
func TestSpecWriteReusesWriterAcrossManyWrites(t *testing.T) {
	repo, tree, parents := specFixtures(t)
	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	w, err := repo.NewCommitWriter(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Close()) }()

	seen := map[git.ObjectID]bool{}
	for i := 0; i < 200; i++ {
		cfg := WriteCommitConfig{
			TreeID:         tree,
			Parents:        parents[:1],
			AuthorName:     "Repeat",
			AuthorEmail:    "repeat@example.com",
			AuthorDate:     time.Unix(1700000000+int64(i), 0).In(time.UTC),
			CommitterName:  "Repeat",
			CommitterEmail: "repeat@example.com",
			CommitterDate:  time.Unix(1700000000+int64(i), 0).In(time.UTC),
			Message:        fmt.Sprintf("message %d\n", i),
		}

		want, err := repo.WriteCommit(ctx, cfg)
		require.NoError(t, err)

		got, err := w.Write(ctx, cfg)
		require.NoError(t, err, "write %d", i)
		require.Equal(t, want, got, "write %d", i)
		require.False(t, seen[got], "write %d produced a duplicate OID", i)
		seen[got] = true
	}
}
