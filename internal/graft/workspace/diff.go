package workspace

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// maxDiffCells bounds the size of the LCS table. Larger files are shown as a
// full replacement instead of computing a minimal diff.
const maxDiffCells = 4_000_000

// Diff returns a full-file, line-based diff of the local changes against the
// base commit. Binary files are reported without content.
func (w *Workspace) Diff() (string, error) {
	changes, err := w.Status()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range changes {
		var oldData, newData []byte
		if c.Kind != ChangeAdded {
			oldData, err = w.object(w.state.Files[c.Path].OID)
			if err != nil {
				return "", err
			}
		}
		if c.Kind != ChangeDeleted {
			full, err := localPath(w.root, c.Path)
			if err != nil {
				return "", err
			}
			newData, err = os.ReadFile(full)
			if err != nil {
				return "", err
			}
		}

		oldName, newName := "a/"+c.Path, "b/"+c.Path
		switch c.Kind {
		case ChangeAdded:
			oldName = "/dev/null"
		case ChangeDeleted:
			newName = "/dev/null"
		}
		fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldName, newName)
		if isBinary(oldData) || isBinary(newData) {
			b.WriteString("Binary files differ\n")
			continue
		}
		writeLineDiff(&b, splitLines(oldData), splitLines(newData))
	}
	return b.String(), nil
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// writeLineDiff writes the longest-common-subsequence diff of a and n, one
// line per entry prefixed with ' ', '-' or '+'.
func writeLineDiff(b *strings.Builder, a, n []string) {
	if len(a)*len(n) > maxDiffCells {
		for _, line := range a {
			b.WriteString("-" + line + "\n")
		}
		for _, line := range n {
			b.WriteString("+" + line + "\n")
		}
		return
	}
	width := len(n) + 1
	// lcs[i*width+j] is the LCS length of a[i:] and n[j:].
	lcs := make([]int, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(n) - 1; j >= 0; j-- {
			if a[i] == n[j] {
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(n) {
		switch {
		case a[i] == n[j]:
			b.WriteString(" " + a[i] + "\n")
			i++
			j++
		case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
			b.WriteString("-" + a[i] + "\n")
			i++
		default:
			b.WriteString("+" + n[j] + "\n")
			j++
		}
	}
	for ; i < len(a); i++ {
		b.WriteString("-" + a[i] + "\n")
	}
	for ; j < len(n); j++ {
		b.WriteString("+" + n[j] + "\n")
	}
}
