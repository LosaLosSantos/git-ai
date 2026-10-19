package corestore_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

const corestorePath = "github.com/feder-cr/git-ai/internal/graft/corestore"

// listedPackage is the part of `go list -json` the test reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Deps       []string
	Module     *struct{ Path string }
}

// goList runs go list -json with args in the module root and decodes the
// packages it prints.
func goList(t *testing.T, root string, args ...string) []listedPackage {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-e", "-json=ImportPath,Dir,GoFiles,Export,Deps,Module"}, args...)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "go list: %s", stderr.String())

	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// TestUploadPackIsTheOnlyUploader enforces invariant U (doc/graft/PLAN7.md,
// RULE 26 and "Removed repositories"): every object of the bucket is written
// by Core.UploadPack, which needs a repository record that a read found. In
// the module's non-test code, type-checked, the calls of Put on a value whose
// static type implements corestore.BlobStore, whatever its name (the field
// Core.Blobs, a variable, a parameter, a wrapper), are exactly one: in
// Core.UploadPack. Only packages that depend on corestore can hold such a
// value. Tests may write the bucket directly to set up states.
func TestUploadPackIsTheOnlyUploader(t *testing.T) {
	root := moduleRoot(t)

	// The module's packages that depend on corestore, and corestore.
	var targets []string
	for _, p := range goList(t, root, "./...") {
		if p.ImportPath == corestorePath || slices.Contains(p.Deps, corestorePath) {
			targets = append(targets, p.ImportPath)
		}
	}
	require.Contains(t, targets, corestorePath)

	// Their export data, and that of everything they import.
	exports := map[string]string{}
	listed := map[string]listedPackage{}
	for _, p := range goList(t, root, append([]string{"-export", "-deps"}, targets...)...) {
		exports[p.ImportPath] = p.Export
		listed[p.ImportPath] = p
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})

	corestore, err := imp.Import(corestorePath)
	require.NoError(t, err)
	blobStore, ok := corestore.Scope().Lookup("BlobStore").Type().Underlying().(*types.Interface)
	require.True(t, ok, "corestore.BlobStore is an interface")

	type put struct{ file, function string }
	var puts []put
	for _, path := range targets {
		p := listed[path]
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			files = append(files, f)
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{Importer: imp}
		pkg, err := conf.Check(path, fset, files, info)
		require.NoError(t, err, "type-check %s", path)
		// Checked from source, corestore has its own BlobStore type (the
		// importer's is that of the export data).
		iface := blobStore
		if path == corestorePath {
			iface = pkg.Scope().Lookup("BlobStore").Type().Underlying().(*types.Interface)
		}

		for _, f := range files {
			for _, decl := range f.Decls {
				function := ""
				if fn, ok := decl.(*ast.FuncDecl); ok {
					function = fn.Name.Name
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Put" {
						return true
					}
					selection, ok := info.Selections[sel]
					if !ok || selection.Kind() != types.MethodVal {
						return true
					}
					recv := selection.Recv()
					if types.Implements(recv, iface) || types.Implements(types.NewPointer(recv), iface) {
						rel, err := filepath.Rel(root, fset.Position(call.Pos()).Filename)
						require.NoError(t, err)
						puts = append(puts, put{file: filepath.ToSlash(rel), function: function})
					}
					return true
				})
			}
		}
	}

	require.Equal(t, []put{{file: "internal/graft/corestore/core.go", function: "UploadPack"}}, puts,
		"every bucket write goes through Core.UploadPack (RULE 26, invariant U)")
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
