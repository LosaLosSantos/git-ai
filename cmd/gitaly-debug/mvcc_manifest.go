package main

import (
	"bufio"
	"os"

	"github.com/feder-cr/git-ai/internal/git/mvcc"
)

func dumpMVCCManifest(manifestFile string) {
	data, err := os.ReadFile(manifestFile)
	noError(err)

	manifest, err := mvcc.ParseManifest(data)
	noError(err)

	out := bufio.NewWriter(os.Stdout)
	defer flush(out)

	hashAlgo := "unknown"
	switch manifest.HashAlgo {
	case 1:
		hashAlgo = "sha1"
	case 2:
		hashAlgo = "sha256"
	}

	objStart, objCount := manifest.ObjRange()
	refPaths := manifest.RefPaths()

	fprintf(out, "# size: %d bytes\n", len(data))
	fprintf(out, "# hash algorithm: %s\n", hashAlgo)
	fprintf(out, "# paths: %d\n", len(manifest.Paths))
	fprintf(out, "# objs: start %d, count %d\n", objStart, objCount)
	fprintf(out, "# refs: %d\n", len(refPaths))

	fprintln(out, "## PATH")
	for i, p := range manifest.Paths {
		fprintf(out, "%d %s\n", i, p)
	}

	fprintln(out, "## OBJS")
	for _, p := range manifest.ObjPaths() {
		fprintln(out, p)
	}

	fprintln(out, "## REFS (stack order, oldest first)")
	for _, p := range refPaths {
		fprintln(out, p)
	}
}
