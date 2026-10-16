package mvcc

import (
	"context"
	"errors"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// recordingVisitor records every artifact it is handed and optionally fails on a chosen path.
type recordingVisitor struct {
	references []Artifact
	objects    []Artifact
	failOn     string
	failErr    error
}

func (v *recordingVisitor) OnReference(_ context.Context, a Artifact) error {
	if a.Name == v.failOn {
		return v.failErr
	}
	v.references = append(v.references, a)
	return nil
}

func (v *recordingVisitor) OnObject(_ context.Context, a Artifact) error {
	if a.Name == v.failOn {
		return v.failErr
	}
	v.objects = append(v.objects, a)
	return nil
}

func TestManifest_walk(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	manifest := &Manifest{
		Paths: []string{
			"pack/abc.idx",
			"pack/abc.pack",
			"pack/abc.rev",
			"refs/def.ref",
			"refs/ghi.ref",
		},
		objStart:      0,
		objCount:      3,
		refStackOrder: []string{"refs/ghi.ref", "refs/def.ref"},
	}

	t.Run("visits references in stack order and objects in manifest order", func(t *testing.T) {
		t.Parallel()

		visitor := &recordingVisitor{}
		require.NoError(t, manifest.walk(ctx, "/cache", visitor))

		require.Equal(t, []Artifact{
			{Name: "refs/ghi.ref", Path: "/cache/refs/ghi.ref"},
			{Name: "refs/def.ref", Path: "/cache/refs/def.ref"},
		}, visitor.references)
		require.Equal(t, []Artifact{
			{Name: "pack/abc.idx", Path: "/cache/pack/abc.idx"},
			{Name: "pack/abc.pack", Path: "/cache/pack/abc.pack"},
			{Name: "pack/abc.rev", Path: "/cache/pack/abc.rev"},
		}, visitor.objects)
	})

	t.Run("empty manifest visits nothing", func(t *testing.T) {
		t.Parallel()

		visitor := &recordingVisitor{}
		require.NoError(t, (&Manifest{}).walk(ctx, "/cache", visitor))
		require.Empty(t, visitor.references)
		require.Empty(t, visitor.objects)
	})

	t.Run("stops at the first visitor error", func(t *testing.T) {
		t.Parallel()

		expectedErr := errors.New("boom")
		visitor := &recordingVisitor{failOn: "refs/def.ref", failErr: expectedErr}

		require.ErrorIs(t, manifest.walk(ctx, "/cache", visitor), expectedErr)
		// The first reference was visited, the failing one aborted the walk before any object.
		require.Len(t, visitor.references, 1)
		require.Empty(t, visitor.objects)
	})

	t.Run("honours context cancellation", func(t *testing.T) {
		t.Parallel()

		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		visitor := &recordingVisitor{}
		require.ErrorIs(t, manifest.walk(cancelledCtx, "/cache", visitor), context.Canceled)
		require.Empty(t, visitor.references)
		require.Empty(t, visitor.objects)
	})
}
