package verify

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/feder-cr/git-ai/sdk"
)

// closers closes several resources in order and joins their errors.
type closers []io.Closer

// Close implements io.Closer.
func (c closers) Close() error {
	var errs []error
	for _, closer := range c {
		errs = append(errs, closer.Close())
	}
	return errors.Join(errs...)
}

// Open connects to the server named by cfg and returns a verifier for the
// configured branch. The returned closer releases the connections; it must be
// called when the verifier is no longer used.
func Open(ctx context.Context, cfg Config, logger *slog.Logger) (*Verifier, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}

	client, err := sdk.Dial(ctx, cfg.Address, sdk.WithStorage(cfg.Storage), sdk.WithToken(cfg.Token))
	if err != nil {
		return nil, nil, err
	}
	repo := client.Repo(cfg.Repository)

	greens, err := dialRefUpdater(ctx, cfg.Address, cfg.Token, repo, cfg.Storage)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}

	// The branch and the green ref are read by their exact names.
	backend := &sdkBackend{repo: repo, refs: greens}
	v, err := newVerifier(ctx, cfg, backend, greens, logger)
	if err != nil {
		_ = greens.Close()
		_ = client.Close()
		return nil, nil, err
	}

	// With automatic reverts enabled, the bisector that finds and reverts the
	// culprit is wired here, so that the graft-verify binary does it too.
	// Without them a failing tip is only reported (see RevertConfig.Enabled).
	if cfg.Revert.Enabled {
		bisector, err := NewBisector(cfg, greens.conn, greens.repository, backend)
		if err != nil {
			_ = greens.Close()
			_ = client.Close()
			return nil, nil, err
		}
		bisector.Logger = v.log
		v.SetBisector(bisector)
	}

	return v, closers{greens, client}, nil
}
