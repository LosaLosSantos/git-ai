package graph

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestWarmerIsRegistered(t *testing.T) {
	t.Parallel()

	require.Contains(t, derived.Warmers(), kindName)
}

// TestWarmerComputesEnqueuedCommits runs the eager warmers of the derived-data pool on
// a repository and checks that the stored entries of the enqueued commit and of its
// history are the ones an Index computes.
func TestWarmerComputesEnqueuedCommits(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	const n = 50
	parents := randomHistory(rand.New(rand.NewSource(3)), n)
	ids := writeCommits(t, cfg, repoPath, parents)

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	store := derived.NewStore(repoPath)
	open := func(ctx context.Context, _ storage.Repository) (derived.WarmEnv, func(), error) {
		reader, cancel, err := catfileCache.ObjectReader(ctx, repo)
		if err != nil {
			return derived.WarmEnv{}, nil, err
		}
		return derived.WarmEnv{Repo: repo, Reader: reader, Store: store}, cancel, nil
	}

	pool := derived.NewPool(testhelper.NewLogger(t), 2, 16, open)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(runCtx)
	}()
	t.Cleanup(func() {
		pool.Close()
		cancel()
		<-done
	})

	last := ids[n-1]
	pool.Enqueue(repoProto, []git.ObjectID{last})

	// The warmers run in the background, so the entries are awaited.
	kind := store.Kind(kindName, kindVersion)
	require.Eventually(t, func() bool {
		_, found, err := kind.Get(last)
		return err == nil && found
	}, 30*time.Second, 10*time.Millisecond, "the graph entry of the enqueued commit is stored")

	expected := generationsOf(parents)
	ancestors := bruteAncestors(hexParents(parents, ids), last)
	for i, id := range ids {
		if !ancestors[id] {
			continue
		}

		data, found, err := kind.Get(id)
		require.NoError(t, err)
		require.True(t, found, "entry of c%d is stored", i)

		e, err := decodeEntry(data)
		require.NoError(t, err)
		require.Equal(t, expected[i], e.generation, "generation of c%d", i)
	}
}

// generationsOf returns the generation of every commit of a history given as indexes.
func generationsOf(parents [][]int) []uint32 {
	generations := make([]uint32, len(parents))
	for i, list := range parents {
		var highest uint32
		for _, parent := range list {
			if generations[parent] > highest {
				highest = generations[parent]
			}
		}
		generations[i] = highest + 1
	}
	return generations
}

// hexParents returns the parents of the commits named by ids, given the parents of the
// history as indexes.
func hexParents(parents [][]int, ids []git.ObjectID) map[git.ObjectID][]git.ObjectID {
	named := make(map[git.ObjectID][]git.ObjectID, len(parents))
	for i, list := range parents {
		for _, parent := range list {
			named[ids[i]] = append(named[ids[i]], ids[parent])
		}
	}
	return named
}
