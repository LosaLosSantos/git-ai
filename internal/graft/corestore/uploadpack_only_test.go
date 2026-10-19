package corestore_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUploadPackIsTheOnlyUploader enforces invariant U (doc/graft/PLAN7.md,
// RULE 26 and "Removed repositories"): every object of the bucket is written
// by Core.UploadPack, which needs a repository record that a read found. The
// bucket is reached through the field Core.Blobs only, so in the module's
// non-test code:
//   - Blobs.Put is called in core.go only (UploadPack);
//   - Blobs is not taken as a value (assigned, passed, returned), which would
//     let a Put happen under another name, except where the core is built.
//
// Tests may write the bucket directly to set up states. The files checked are
// those that can reach a *corestore.Core: they import corestore or one of its
// packages, call the dependencies' GetGraftStorage, or are in internal/graft
// or the storagecore service (other fields named Blobs, in protobuf messages
// and git packfile code, are unrelated).
func TestUploadPackIsTheOnlyUploader(t *testing.T) {
	root := moduleRoot(t)
	// Where Blobs may be used as a value: the core is built and closed there.
	wiring := map[string]bool{
		"internal/graft/corestore/core.go":      true,
		"internal/graft/corestore/open/open.go": true,
	}

	var violations []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "_build" || d.Name() == "_support" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !reachesCore(rel, file, src) {
			return nil
		}

		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Blobs" {
				return true
			}
			pos := fset.Position(sel.Pos())
			// The parent of x.Blobs: a method selection (x.Blobs.M) or a use as
			// a value.
			if len(stack) >= 2 {
				if method, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && method.X == sel {
					if method.Sel.Name == "Put" && rel != "internal/graft/corestore/core.go" {
						violations = append(violations, pos.String()+": Blobs.Put outside Core.UploadPack")
					}
					return true
				}
			}
			if !wiring[rel] {
				violations = append(violations, pos.String()+": Blobs used as a value outside the core's wiring")
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, violations, "every bucket write goes through Core.UploadPack (RULE 26, invariant U)")
}

// reachesCore reports whether the file at rel can reach a *corestore.Core.
func reachesCore(rel string, file *ast.File, src []byte) bool {
	if strings.HasPrefix(rel, "internal/graft/") || strings.HasPrefix(rel, "internal/gitaly/service/storagecore/") {
		return true
	}
	for _, imp := range file.Imports {
		if strings.HasPrefix(strings.Trim(imp.Path.Value, `"`), "github.com/feder-cr/git-ai/internal/graft/corestore") {
			return true
		}
	}
	return strings.Contains(string(src), "GetGraftStorage")
}

// moduleRoot returns the directory of the module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test")
		dir = parent
	}
}
