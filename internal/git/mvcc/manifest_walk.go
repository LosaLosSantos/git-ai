package mvcc

import (
	"context"
	"fmt"
	"path/filepath"
)

// Artifact represents an MVCC artifact
type Artifact struct {
	// Name is the artifact's name as recorded in the manifest.
	Name string
	// Path is the absolute path of the artifact on the local disk.
	Path string
}

// ArtifactVisitor receives the artifacts of a Manifest as walk visits them. Returning
// an error from any method aborts the walk and walk returns that error.
type ArtifactVisitor interface {
	// OnReference is called once per reftable artifact, in stack order (oldest first).
	OnReference(ctx context.Context, artifact Artifact) error
	// OnObject is called once per object artifact.
	OnObject(ctx context.Context, artifact Artifact) error
}

// walk visits every reftable and object artifact in the manifest.
func (m *Manifest) walk(ctx context.Context, cacheDir string, visitor ArtifactVisitor) error {
	visit := func(names []string, fn func(context.Context, Artifact) error) error {
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}

			if err := fn(ctx, Artifact{
				Name: name,
				Path: filepath.Join(cacheDir, filepath.FromSlash(name)),
			}); err != nil {
				return err
			}
		}

		return nil
	}

	if err := visit(m.RefPaths(), visitor.OnReference); err != nil {
		return fmt.Errorf("visit ref: %w", err)
	}

	if err := visit(m.ObjPaths(), visitor.OnObject); err != nil {
		return fmt.Errorf("visit object: %w", err)
	}

	return nil
}
