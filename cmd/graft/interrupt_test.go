package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// cancelAwareBackend fails server calls once their context is canceled, as the
// sdk backend does.
type cancelAwareBackend struct {
	*workspace.MemoryBackend
}

func (b cancelAwareBackend) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return b.MemoryBackend.ResolveRevision(ctx, rev)
}

func TestRunInterruptedCommandReleasesWorkspace(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return cancelAwareBackend{mem}, nil }
	t.Cleanup(func() { backendFactory = previous })

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, errOut bytes.Buffer
	code = runContext(ctx, []string{"update", "-C", dir}, &stdout, &errOut)
	require.Equal(t, exitInterrupted, code)
	require.Equal(t, "graft update: interrupted\n", errOut.String())

	// The interrupted command does not leave the workspace locked.
	code, out, stderr := runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "nothing changed\n", out)
}
