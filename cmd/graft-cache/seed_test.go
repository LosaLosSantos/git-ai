package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSnapshotOptions(t *testing.T) {
	t.Parallel()

	t.Run("all flags", func(t *testing.T) {
		opts, err := parseSnapshotOptions([]string{
			"--upstream", "unix:/run/gitaly.sock",
			"--token", "secret",
			"--storage", "shard-1",
			"--repo", "demo.git",
			"--rev", "main",
			"--out", "/var/lib/graft/demo.seed",
		}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, snapshotOptions{
			upstream: "unix:/run/gitaly.sock",
			token:    "secret",
			storage:  "shard-1",
			repo:     "demo.git",
			rev:      "main",
			out:      "/var/lib/graft/demo.seed",
		}, opts)
	})

	t.Run("storage defaults to default", func(t *testing.T) {
		opts, err := parseSnapshotOptions([]string{
			"--upstream", "tcp://127.0.0.1:9999", "--repo", "demo.git", "--rev", "main", "--out", "demo.seed",
		}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "default", opts.storage)
		require.Empty(t, opts.token)
	})

	for name, args := range map[string][]string{
		"missing upstream": {"--repo", "demo.git", "--rev", "main", "--out", "demo.seed"},
		"missing repo":     {"--upstream", "unix:/a.sock", "--rev", "main", "--out", "demo.seed"},
		"missing rev":      {"--upstream", "unix:/a.sock", "--repo", "demo.git", "--out", "demo.seed"},
		"missing out":      {"--upstream", "unix:/a.sock", "--repo", "demo.git", "--rev", "main"},
		"empty storage":    {"--upstream", "unix:/a.sock", "--storage", "", "--repo", "demo.git", "--rev", "main", "--out", "demo.seed"},
		"positional args":  {"--upstream", "unix:/a.sock", "--repo", "demo.git", "--rev", "main", "--out", "demo.seed", "extra"},
		"unknown flag":     {"--bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseSnapshotOptions(args, io.Discard)
			require.Error(t, err)
		})
	}
}

func TestSplitSeedFiles(t *testing.T) {
	t.Parallel()

	require.Nil(t, splitSeedFiles(""))
	require.Equal(t, []string{"a.seed"}, splitSeedFiles("a.seed"))
	require.Equal(t, []string{"a.seed", "b.seed"}, splitSeedFiles(" a.seed, ,b.seed,"))
}
