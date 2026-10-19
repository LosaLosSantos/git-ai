package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/feder-cr/git-ai/internal/graft/merge3"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The conflicts directory of graft land --conflicts-dir DIR holds, for every conflicting path:
//
//	DIR/<path>                         the file as the merge leaves it, with git's diff3 conflict
//	                                   markers around each conflict (or the tip's side when the
//	                                   file is not text, or a side is absent)
//	DIR/.graft/sides/<path>.base       the three sides
//	DIR/.graft/sides/<path>.ours
//	DIR/.graft/sides/<path>.theirs
//	DIR/.graft/conflict.json           the conflict package
//
// graft land --resolved DIR lands the files of DIR as the resolution: a path whose file is present
// is written, one whose file is absent is deleted.
const (
	conflictsMetaDir  = ".graft"
	conflictsPackage  = "conflict.json"
	conflictsSidesDir = "sides"
)

// printConflict describes a conflict package: the reason the server gave, and each file with its
// outcome. The paths and reasons come from the server and are escaped with displayText.
func printConflict(out io.Writer, c *gitalypb.LandConflict) {
	fmt.Fprintf(out, "the land did not resolve %d conflicting files (tip %s, commit %s)\n", len(c.GetFiles()), shortOID(c.GetTip()), shortOID(c.GetCommit()))
	if c.GetReason() != "" {
		fmt.Fprintf(out, "  %s\n", displayText(c.GetReason()))
	}
	for _, f := range c.GetFiles() {
		outcome := strings.ToLower(strings.TrimPrefix(f.GetOutcome().String(), "OUTCOME_"))
		line := fmt.Sprintf("  %s: %s", displayText(string(f.GetPath())), outcome)
		if f.GetReason() != "" {
			line += ": " + displayText(f.GetReason())
		}
		fmt.Fprintln(out, line)
	}
}

// conflictPath returns the file of the conflicts directory for a conflicting path, refusing a
// path that would leave the directory.
func conflictPath(dir, p string) (string, error) {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || p == conflictsMetaDir || strings.HasPrefix(p, conflictsMetaDir+"/") {
		return "", fmt.Errorf("conflicting path %q cannot be written into the conflicts directory", p)
	}
	return filepath.Join(dir, filepath.FromSlash(p)), nil
}

// writeConflicts writes the conflicts directory of a conflict package (see the layout above).
func writeConflicts(ctx context.Context, ws *workspace.Workspace, c *gitalypb.LandConflict, dir string) error {
	var oids []string
	for _, f := range c.GetFiles() {
		for _, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if s != nil {
				oids = append(oids, s.GetOid())
			}
		}
	}
	blobs, err := ws.GetObjects(ctx, oids)
	if err != nil {
		return fmt.Errorf("reading the sides of the conflict: %w", err)
	}
	side := func(s *gitalypb.LandConflictSide) ([]byte, error) {
		if s == nil {
			return nil, nil
		}
		b, ok := blobs[s.GetOid()]
		if !ok {
			return nil, fmt.Errorf("side %s is missing from the server", s.GetOid())
		}
		if b == nil {
			b = []byte{}
		}
		return b, nil
	}

	for _, f := range c.GetFiles() {
		p := string(f.GetPath())
		target, err := conflictPath(dir, p)
		if err != nil {
			return err
		}
		var contents [3][]byte
		for i, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if contents[i], err = side(s); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		for i, ext := range []string{"base", "ours", "theirs"} {
			if contents[i] == nil {
				continue
			}
			sidePath := filepath.Join(dir, conflictsMetaDir, conflictsSidesDir, filepath.FromSlash(p)+"."+ext)
			if err := writeFileAll(sidePath, contents[i]); err != nil {
				return err
			}
		}
		if err := writeFileAll(target, conflictFileContent(contents)); err != nil {
			return err
		}
	}

	raw, err := protojson.MarshalOptions{Multiline: true}.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAll(filepath.Join(dir, conflictsMetaDir, conflictsPackage), append(raw, '\n'))
}

// conflictFileContent is the file an agent edits: the merge with markers when both sides are text,
// otherwise the tip's side (or the landed commit's when the tip deleted the file).
func conflictFileContent(sides [3][]byte) []byte {
	base, ours, theirs := sides[0], sides[1], sides[2]
	text := ours != nil && theirs != nil && !isBinaryContent(base) && !isBinaryContent(ours) && !isBinaryContent(theirs)
	if text {
		if r, ok := merge3.Merge(base, ours, theirs); ok {
			return r.WithMarkers(merge3.Labels{Ours: "ours (the branch tip)", Base: "base", Theirs: "theirs (the change)"})
		}
	}
	if ours != nil {
		return ours
	}
	return theirs
}

func isBinaryContent(b []byte) bool {
	return bytes.IndexByte(b, 0) >= 0
}

func writeFileAll(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// readResolution reads the resolution of a conflicts directory: the package's tip and commit, and
// for every conflicting path its file, or a deletion when the file is absent. The mode is the
// tip's side's, else the landed commit's.
func readResolution(dir string) (*workspace.Resolution, error) {
	raw, err := os.ReadFile(filepath.Join(dir, conflictsMetaDir, conflictsPackage))
	if err != nil {
		return nil, fmt.Errorf("reading the conflict package of %s (written by graft land --conflicts-dir): %w", dir, err)
	}
	var c gitalypb.LandConflict
	if err := protojson.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, conflictsMetaDir, conflictsPackage), err)
	}
	res := &workspace.Resolution{Tip: c.GetTip(), Commit: c.GetCommit()}
	for _, f := range c.GetFiles() {
		p := string(f.GetPath())
		target, err := conflictPath(dir, p)
		if err != nil {
			return nil, err
		}
		content, err := os.ReadFile(target)
		if errors.Is(err, fs.ErrNotExist) {
			res.Files = append(res.Files, workspace.ResolvedFile{Path: p, Delete: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		mode := f.GetOurs().GetMode()
		if mode == "" {
			mode = f.GetTheirs().GetMode()
		}
		if mode == "" {
			mode = "100644"
		}
		res.Files = append(res.Files, workspace.ResolvedFile{Path: p, Mode: mode, Content: content})
	}
	return res, nil
}
