package syncer_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestEnsureFreshWaitsForItsOwnRound: a call that arrives during a sync round
// of its repository joins it and, as that round read the head before the call
// arrived, then waits for a round of its own. Each round runs under its own
// sync_timeout. The call must not give up while its own round is within that
// budget, even when, with the round it joined, it waited longer than one
// sync_timeout since it arrived.
//
// The rounds are made slow at their end (the "synced" point, after the sync
// itself returned): the joined round ends 0.7 x sync_timeout after the call
// arrived, and the call's own round 1.2 x sync_timeout after it, which is
// 0.5 x sync_timeout into that round.
func TestEnsureFreshWaitsForItsOwnRound(t *testing.T) {
	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c1 := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c1)

	b := a.secondNode(t, sqlstore.SQLite)
	const syncTimeout = 2 * time.Second
	b.core.SyncTimeout = syncTimeout

	var (
		rounds         atomic.Int32
		secondArrived  atomic.Int64
		firstRoundHeld = make(chan struct{})
		releaseFirst   = make(chan struct{})
	)
	b.sy.SetTestHook(func(point string) {
		if point != "synced" {
			return
		}
		switch rounds.Add(1) {
		case 1:
			close(firstRoundHeld)
			<-releaseFirst
		case 2:
			time.Sleep(time.Until(time.Unix(0, secondArrived.Load()).Add(syncTimeout * 12 / 10)))
		}
	})

	first := make(chan error, 1)
	go func() { first <- b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)) }()
	<-firstRoundHeld

	// The second call arrives while the first round is held after it read the head.
	arrived := time.Now()
	secondArrived.Store(arrived.UnixNano())
	second := make(chan error, 1)
	go func() { second <- b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)) }()

	time.Sleep(time.Until(arrived.Add(syncTimeout * 7 / 10)))
	close(releaseFirst)

	require.NoError(t, <-first)
	require.NoError(t, <-second, "the call gave up while its own round was within its budget")
	require.Greater(t, time.Since(arrived), syncTimeout, "the call waited longer than one sync_timeout since it arrived")
	require.Equal(t, int32(2), rounds.Load(), "the second call ran a round of its own")
}
