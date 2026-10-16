package gitcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
)

var (
	sectionRegex    = regexp.MustCompile(`^[ \t]*\[[ \t]*([^\] \t]+)`)
	refstorageRegex = regexp.MustCompile(`(?im)^\s*refstorage\s*=\s*([^\s#;]+)`)
)

// referenceBackendCtxKey is the context key under which the repository's reference backend is
// stored. It is used to select the bundled Git binary that matches a repository's reference
// backend, most importantly the MVCC-enabled binary.
type referenceBackendCtxKey struct{}

// ContextWithReferenceBackend returns a new context that carries the given reference backend. The
// reference backend is used by the command factory to select the bundled Git binary appropriate
// for a repository's reference backend.
func ContextWithReferenceBackend(ctx context.Context, backend git.ReferenceBackend) context.Context {
	return context.WithValue(ctx, referenceBackendCtxKey{}, backend)
}

// referenceBackendFromContext returns the reference backend stored in the context, if any.
func referenceBackendFromContext(ctx context.Context) (git.ReferenceBackend, bool) {
	backend, ok := ctx.Value(referenceBackendCtxKey{}).(git.ReferenceBackend)
	return backend, ok
}

// DetectReferenceBackend detects the reference backend used by the repository.
// It plucks out the first refstorage configuration value from the repository's configuration
// file and doesn't validate the configuration.
//
// Note: It is recommended to use localrepo.ReferenceBackend since that value is
// cached for a given repository.
func DetectReferenceBackend(ctx context.Context, repoPath string) (_ git.ReferenceBackend, returnedErr error) {
	configFile, err := os.Open(filepath.Join(repoPath, "config"))
	if err != nil {
		return git.ReferenceBackend{}, fmt.Errorf("open: %w", err)
	}
	defer func() {
		if err := configFile.Close(); err != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("close: %w", err))
		}
	}()

	// We default to the files backend and override it with the first refstorage value found in
	// the configuration. Only the reference backend is relevant here, as it alone determines
	// which bundled Git binary is able to read the repository. We deliberately do not inspect the
	// object storage extension: a repository using the MVCC reference backend must be served by
	// the MVCC binary regardless of how its objects are stored.
	refStorage := git.ReferenceBackendFiles.Name

	scanner := bufio.NewScanner(configFile)

	inExtensions := false
	for scanner.Scan() {
		line := scanner.Text()
		if m := sectionRegex.FindStringSubmatch(line); m != nil {
			inExtensions = strings.EqualFold(m[1], "extensions")
			continue
		}
		if !inExtensions {
			continue
		}
		if m := refstorageRegex.FindStringSubmatch(line); m != nil {
			refStorage = strings.TrimSpace(m[1])
		}
	}

	return git.ReferenceBackendByName(refStorage)
}
