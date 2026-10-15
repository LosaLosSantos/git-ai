package localrepo

import (
	"context"
	"io"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
)

// WriteBlobConfig is the configuration used to write blobs into the repository.
type WriteBlobConfig = gitcmd.WriteBlobConfig

// WriteBlob writes a blob to the repository's object database and
// returns its object ID.
func (repo *Repo) WriteBlob(ctx context.Context, content io.Reader, cfg WriteBlobConfig) (git.ObjectID, error) {
	return gitcmd.WriteBlob(ctx, repo, content, cfg)
}
