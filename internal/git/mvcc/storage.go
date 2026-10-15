package mvcc

import (
	"context"
	"errors"
	"io"
)

// ErrManifestPointerConflict is returned by an ObjectStorage's Commit when the
// manifest pointer has diverged from the base hash that was resolved at the
// start of the RPC, i.e. another writer advanced the pointer concurrently.
// Conflict resolution (e.g. a three-way merge) is out of scope for now, so the
// RPC must fail; clients may retry against the new base.
var ErrManifestPointerConflict = errors.New("mvcc: manifest pointer conflict")

// ManifestPointer identifies the base state a mutator RPC builds on: the manifest
// hash resolved at the start of the RPC and, when the backend tracks it, the
// generation that hash occupies in the log. Carrying the generation lets Commit
// target the next slot without re-reading the head. HasGeneration is false when
// only a hash is known (a read pin, or a backend without generations such as
// LocalStorage), which makes Commit resolve the head itself.
type ManifestPointer struct {
	// Hash is the content-addressed manifest hash. It is empty when the
	// repository has no committed manifest yet (a genesis base).
	Hash string
	// Generation is the position of Hash in the append-only generation log.
	// Only meaningful when HasGeneration is true.
	Generation uint64
	// HasGeneration reports whether Generation is known and can be used to skip
	// the head read at commit time.
	HasGeneration bool
}

// ObjectStorage is the interface used for a component to communicate with object storage.
type ObjectStorage interface {
	// ReadManifestPointer reads the manifest pointer for the given repository ID.
	// Backends that track generations populate ManifestPointer.Generation so a
	// later Commit can advance without re-reading the head.
	ReadManifestPointer(ctx context.Context, repositoryID string) (ManifestPointer, error)

	// ReadManifest reads a specific manifest for the repository, keyed on the hash.
	ReadManifest(ctx context.Context, repositoryID string, hash string) (io.ReadCloser, error)

	// ReadArtifact ensures the artifact identified by `key` is fetched for the given repository.
	// The consume callback is invoked with an io.ReadCloser that must be used to read the artifact.
	ReadArtifact(ctx context.Context, repositoryID, key string, consume func(ctx context.Context, r io.ReadCloser) error) error

	// PutArtifact writes `content` at `key` for the given repository.
	PutArtifact(ctx context.Context, repositoryID, key string, content io.ReadCloser) error

	// Commit commits the new manifest pointer for the given repository. This operation
	// must be atomic, by comparing base (the state resolved at the start of the RPC)
	// with the newHash. If a conflict occurs, some implementations can introduce a conflict
	// resolution mechanism (ex: three-way merge).
	Commit(ctx context.Context, repositoryID string, base ManifestPointer, newHash string) error

	// TearDown releases all resources opened by the Storage. It is good practice to
	// run when the Storage is no longer needed.
	TearDown(ctx context.Context) error
}
