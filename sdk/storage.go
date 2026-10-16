package sdk

import (
	"context"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// StorageImport is the outcome of importing a repository into the storage core.
//
// A first import records the references in changeset 1 and copies the history
// of the files. The import of a repository that is already imported (a
// history import) only brings in the changelog entries and the reviews
// written into the files since then: References and PackBytes are 0 and
// Sequence is the current head. References is never 0 for a first import, which
// always records HEAD.
type StorageImport struct {
	// Sequence is the sequence of the import changeset (1) for a first import,
	// and the current head sequence for a history import.
	Sequence uint64 `json:"sequence"`
	// References is the number of references imported, HEAD included; 0 for a
	// history import, which does not touch the references.
	References uint32 `json:"references"`
	// PackBytes is the size of the uploaded pack; 0 for a history import.
	PackBytes int64 `json:"pack_bytes"`
	// ChangelogEntries is the number of changelog entries imported.
	ChangelogEntries uint32 `json:"changelog_entries"`
	// Reviews is the number of reviews imported.
	Reviews uint32 `json:"reviews"`
}

// StorageState is how the storage core sees a repository and how far this
// node's local copy is.
type StorageState struct {
	// Mode is the storage core mode of the node: "off", "shadow" or "authoritative".
	Mode string `json:"mode"`
	// Tracked is true when the database knows the repository.
	Tracked bool `json:"tracked"`
	// RepositoryID is the database ID of the repository, 0 when untracked.
	RepositoryID int64 `json:"repository_id"`
	// HeadSequence is the sequence of the repository's last changeset.
	HeadSequence uint64 `json:"head_sequence"`
	// AppliedSequence is how far the local copy is, 0 when it has no watermark.
	AppliedSequence uint64 `json:"applied_sequence"`
	// NodeID is the name of the node in the database.
	NodeID string `json:"node_id"`
}

// StorageDivergence is a reference whose local value differs from the database.
type StorageDivergence struct {
	// Reference is the fully qualified reference name, or HEAD.
	Reference string `json:"reference"`
	// LocalTarget is the local value: an object ID, "ref:<name>" for a symbolic
	// reference, empty when absent.
	LocalTarget string `json:"local_target"`
	// DatabaseTarget is the database value, in the same format.
	DatabaseTarget string `json:"database_target"`
}

// StorageVerify is the outcome of comparing a repository's local references
// with the database.
type StorageVerify struct {
	// HeadSequence is the database sequence the references were compared at.
	HeadSequence uint64 `json:"head_sequence"`
	// Divergences lists every reference that differs; empty when consistent.
	Divergences []StorageDivergence `json:"divergences"`
	// ConnectivityOK is true when the connectivity check ran and passed.
	ConnectivityOK bool `json:"connectivity_ok"`
	// ConnectivityError describes a failed connectivity check.
	ConnectivityError string `json:"connectivity_error"`
}

// StorageImport imports the repository into the storage core: its references,
// one pack of every reachable object, its changelog history and its reviews.
// On a repository that is already imported it imports the history written into
// the files since then instead (the file changelog entries beyond the
// database's last sequence and the file reviews the database does not have),
// without touching the references; running it again imports nothing new. It
// fails with AlreadyExists when the server's local copy is not the one that
// was imported (its watermark names another repository ID, or it has none),
// or when another import of the repository is in progress, and with
// FailedPrecondition when the storage core is off.
func (r *Repo) StorageImport(ctx context.Context) (*StorageImport, error) {
	resp, err := r.storageCore().ImportRepository(ctx, &gitalypb.ImportRepositoryRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return nil, err
	}
	return &StorageImport{
		Sequence:         resp.GetSequence(),
		References:       resp.GetReferences(),
		PackBytes:        resp.GetPackBytes(),
		ChangelogEntries: resp.GetChangelogEntries(),
		Reviews:          resp.GetReviews(),
	}, nil
}

// StorageState returns how the storage core sees the repository and how far
// the local copy on the server is.
func (r *Repo) StorageState(ctx context.Context) (*StorageState, error) {
	resp, err := r.storageCore().GetStorageState(ctx, &gitalypb.GetStorageStateRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return nil, err
	}
	return &StorageState{
		Mode:            resp.GetMode(),
		Tracked:         resp.GetTracked(),
		RepositoryID:    resp.GetRepositoryId(),
		HeadSequence:    resp.GetHeadSequence(),
		AppliedSequence: resp.GetAppliedSequence(),
		NodeID:          resp.GetNodeId(),
	}, nil
}

// StorageVerify compares the local references of the repository with the
// database. With connectivity set, it also checks the connectivity of the
// local repository and that the object storage holds every object the
// database references reach (the server downloads every pack of the
// repository for that). In authoritative mode the server catches the local
// repository up first.
func (r *Repo) StorageVerify(ctx context.Context, connectivity bool) (*StorageVerify, error) {
	resp, err := r.storageCore().VerifyRepository(ctx, &gitalypb.VerifyRepositoryRequest{
		Repository:        r.repository(),
		CheckConnectivity: connectivity,
	})
	if err != nil {
		return nil, err
	}

	out := &StorageVerify{
		HeadSequence: resp.GetHeadSequence(),
		// Never nil, so that the JSON form lists no divergences as [].
		Divergences:       make([]StorageDivergence, 0, len(resp.GetDivergences())),
		ConnectivityOK:    resp.GetConnectivityOk(),
		ConnectivityError: resp.GetConnectivityError(),
	}
	for _, d := range resp.GetDivergences() {
		out.Divergences = append(out.Divergences, StorageDivergence{
			Reference:      string(d.GetReference()),
			LocalTarget:    d.GetLocalTarget(),
			DatabaseTarget: d.GetDatabaseTarget(),
		})
	}
	return out, nil
}

// storageCore returns the storage core service client on the connection of the client.
func (r *Repo) storageCore() gitalypb.StorageCoreServiceClient {
	return gitalypb.NewStorageCoreServiceClient(r.client.conn)
}
