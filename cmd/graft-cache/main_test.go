package main

import (
	"os"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/cachetier"
	"github.com/stretchr/testify/require"
)

func TestParseOptions(t *testing.T) {
	t.Parallel()

	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = devNull.Close() })

	t.Run("defaults", func(t *testing.T) {
		opts, err := parseOptions([]string{"--listen", "unix:/tmp/a.sock", "--upstream", "unix:/tmp/gitaly.sock"}, devNull)
		require.NoError(t, err)
		require.Equal(t, "cache", opts.tier)
		require.Equal(t, int64(defaultMaxBytes), opts.maxBytes)
		require.Equal(t, config.DefaultGraftChunkSize, opts.chunkSize)
		require.Equal(t, int64(cachetier.DefaultDiskMaxBytes), opts.diskMaxBytes)
		require.Empty(t, opts.diskDir)
		require.Empty(t, opts.metricsAddr)
	})

	t.Run("all flags", func(t *testing.T) {
		opts, err := parseOptions([]string{
			"--listen", "tcp://127.0.0.1:9000",
			"--upstream", "tcp://127.0.0.1:9999",
			"--tier", "daemon",
			"--max-bytes", "1024",
			"--chunk-size", "4096",
			"--disk-dir", "/var/cache/graft",
			"--disk-max-bytes", "2048",
			"--metrics-addr", "127.0.0.1:9236",
			"--token", "secret",
		}, devNull)
		require.NoError(t, err)
		require.Equal(t, "daemon", opts.tier)
		require.Equal(t, int64(1024), opts.maxBytes)
		require.Equal(t, 4096, opts.chunkSize)
		require.Equal(t, "/var/cache/graft", opts.diskDir)
		require.Equal(t, int64(2048), opts.diskMaxBytes)
		require.Equal(t, "127.0.0.1:9236", opts.metricsAddr)
		require.Equal(t, "secret", opts.token)
	})

	for name, args := range map[string][]string{
		"missing listen":      {"--upstream", "unix:/tmp/gitaly.sock"},
		"missing upstream":    {"--listen", "unix:/tmp/a.sock"},
		"positional args":     {"--listen", "unix:/tmp/a.sock", "--upstream", "unix:/tmp/g.sock", "extra"},
		"unknown flag":        {"--listen", "unix:/tmp/a.sock", "--upstream", "unix:/tmp/g.sock", "--bogus"},
		"zero chunk size":     {"--listen", "unix:/tmp/a.sock", "--upstream", "unix:/tmp/g.sock", "--chunk-size", "0"},
		"zero disk max bytes": {"--listen", "unix:/tmp/a.sock", "--upstream", "unix:/tmp/g.sock", "--disk-max-bytes", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseOptions(args, devNull)
			require.Error(t, err)
		})
	}
}

func TestListenNetworkAndAddress(t *testing.T) {
	t.Parallel()

	network, address, err := listenNetworkAndAddress("unix:/run/graft-cache.sock")
	require.NoError(t, err)
	require.Equal(t, "unix", network)
	require.Equal(t, "/run/graft-cache.sock", address)

	network, address, err = listenNetworkAndAddress("tcp://127.0.0.1:9000")
	require.NoError(t, err)
	require.Equal(t, "tcp", network)
	require.Equal(t, "127.0.0.1:9000", address)

	for _, listen := range []string{"127.0.0.1:9000", "unix:", "tcp://", "http://x"} {
		_, _, err := listenNetworkAndAddress(listen)
		require.Error(t, err, listen)
	}
}
