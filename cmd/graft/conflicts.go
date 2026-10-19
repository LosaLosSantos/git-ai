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
//	DIR/files/<path>          the file as the merge leaves it, with git's diff3 conflict markers
//	                          around each conflict (or the side there is when the file is not
//	                          text, or a side is absent or cannot be read)
//	DIR/sides/base/<path>     the three sides; a side that is absent or cannot be read has no file
//	DIR/sides/ours/<path>
//	DIR/sides/theirs/<path>
//	DIR/conflict.json         the conflict package
//
// Each directory mirrors the tree of the merge, so that no repository path, whatever its name, can
// collide with a side, with another path's side, or with the package. graft land --resolved DIR
// lands the files of DIR/files as the resolution: a path whose file is present is written, one
// whose file is absent is deleted.
const (
	conflictsFilesDir = "files"
	conflictsSidesDir = "sides"
	conflictsPackage  = "conflict.json"
)

// conflictSides are the sides of a conflicting file, in the order of the package.
var conflictSides = []string{"base", "ours", "theirs"}

// conflictsUnreadable is the content of the file to edit when no side of the file can be read: a
// conflict that the marker rule of the server rejects until the agent writes the resolution.
const conflictsUnreadable = "<<<<<<< ours (the branch tip): cannot be read, see conflict.json\n=======\n>>>>>>> theirs (the change): cannot be read, see conflict.json\n"

// printConflict describes a conflict package: the reason the server gave, each file with its
// outcome and what it lacks, and the merge's messages. The paths, reasons and messages come from
// the server and are escaped with displayText.
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
		for i, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if s.GetUnavailable() != "" {
				fmt.Fprintf(out, "    %s side unavailable: %s\n", conflictSides[i], displayText(s.GetUnavailable()))
			}
		}
		if f.GetHunksOmitted() != "" {
			fmt.Fprintf(out, "    hunks: %s\n", displayText(f.GetHunksOmitted()))
		}
	}
	for _, m := range c.GetMessages() {
		fmt.Fprintf(out, "  merge: %s\n", displayText(m.GetMessage()))
	}
}

// conflictPath returns the file under sub of the conflicts directory for a conflicting path. It
// refuses a path that would leave the directory or that this system cannot hold as a file name
// under it: an absolute or unclean path, a backslash or a colon (a separator or a drive or stream
// on Windows), a NUL, and any path filepath.IsLocal rejects once in the system's form (such as a
// reserved name like NUL or COM1 on Windows).
func conflictPath(dir, sub, p string) (string, error) {
	local := filepath.FromSlash(p)
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\:\x00") || !filepath.IsLocal(local) {
		return "", fmt.Errorf("conflicting path %q cannot be written into the conflicts directory on this system", p)
	}
	return filepath.Join(dir, sub, local), nil
}

// writeConflicts writes the conflicts directory of a conflict package (see the layout above) into
// dir, which must not hold one yet: the files of an earlier package, which the agent may be
// editing, are never overwritten. A side that has no object ID in the package (unavailable) or that
// the server cannot return is left out, and said so; it fails neither its file nor the directory.
// Two paths that this file system takes for the same file (such as names that differ in case on a
// case-insensitive one) are an error, since one would overwrite the other.
func writeConflicts(ctx context.Context, ws *workspace.Workspace, c *gitalypb.LandConflict, dir string, out io.Writer) error {
	for _, sub := range []string{conflictsFilesDir, conflictsSidesDir, conflictsPackage} {
		if _, err := os.Lstat(filepath.Join(dir, sub)); err == nil {
			return fmt.Errorf("%s already holds a conflicts directory (%s): land it with --resolved, or remove it", dir, sub)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	blobs, missing := fetchSides(ctx, ws, c)
	written := map[string]string{}
	create := func(target, p string, data []byte) error {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("conflicting paths %q and %q are the same file on this file system (names that differ only in case or Unicode normalization): resolve this conflict on a case-sensitive file system", written[target], p)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		written[target] = p
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return fmt.Errorf("%s: %w", p, err)
		}
		return f.Close()
	}

	for _, f := range c.GetFiles() {
		p := string(f.GetPath())
		target, err := conflictPath(dir, conflictsFilesDir, p)
		if err != nil {
			return err
		}
		var contents [3][]byte
		readable := true
		for i, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if s == nil {
				continue
			}
			b, ok := blobs[s.GetOid()]
			switch {
			case s.GetOid() == "":
				readable = false
				continue
			case !ok:
				readable = false
				fmt.Fprintf(out, "  %s: the %s side %s cannot be read: %s\n", displayText(p), conflictSides[i], shortOID(s.GetOid()), displayText(missing[s.GetOid()]))
				continue
			}
			if b == nil {
				b = []byte{}
			}
			contents[i] = b
			sideFile, err := conflictPath(dir, filepath.Join(conflictsSidesDir, conflictSides[i]), p)
			if err != nil {
				return err
			}
			if err := create(sideFile, p, b); err != nil {
				return err
			}
		}
		content := conflictFileContent(contents)
		if !readable {
			// Without every side there is no three-way merge: the file to edit is a side that
			// can be read, or a conflict the agent must resolve.
			switch {
			case contents[1] != nil:
				content = contents[1]
			case contents[2] != nil:
				content = contents[2]
			default:
				content = []byte(conflictsUnreadable)
			}
		}
		if err := create(target, p, content); err != nil {
			return err
		}
	}

	raw, err := protojson.MarshalOptions{Multiline: true}.Marshal(c)
	if err != nil {
		return err
	}
	return create(filepath.Join(dir, conflictsPackage), conflictsPackage, append(raw, '\n'))
}

// fetchSides reads the sides of the package that have an object ID, all at once. When the server
// cannot return them all, it reads them one by one, so that a side it cannot return costs only that
// side: missing gives the error of each side that could not be read.
func fetchSides(ctx context.Context, ws *workspace.Workspace, c *gitalypb.LandConflict) (map[string][]byte, map[string]string) {
	var oids []string
	seen := map[string]bool{}
	for _, f := range c.GetFiles() {
		for _, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if oid := s.GetOid(); oid != "" && !seen[oid] {
				seen[oid] = true
				oids = append(oids, oid)
			}
		}
	}
	missing := map[string]string{}
	blobs, err := ws.GetObjects(ctx, oids)
	if err == nil {
		for _, oid := range oids {
			if _, ok := blobs[oid]; !ok {
				missing[oid] = "the server did not return it"
			}
		}
		return blobs, missing
	}
	blobs = map[string][]byte{}
	for _, oid := range oids {
		one, err := ws.GetObjects(ctx, []string{oid})
		if b, ok := one[oid]; err == nil && ok {
			blobs[oid] = b
			continue
		}
		if err == nil {
			err = errors.New("the server did not return it")
		}
		missing[oid] = err.Error()
	}
	return blobs, missing
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

// readResolution reads the resolution of a conflicts directory: the package's tip and commit, and
// for every conflicting path its file under DIR/files, or a deletion when the file is absent. The mode is the
// tip's side's, else the landed commit's.
func readResolution(dir string) (*workspace.Resolution, error) {
	raw, err := os.ReadFile(filepath.Join(dir, conflictsPackage))
	if err != nil {
		return nil, fmt.Errorf("reading the conflict package of %s (written by graft land --conflicts-dir): %w", dir, err)
	}
	var c gitalypb.LandConflict
	if err := protojson.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, conflictsPackage), err)
	}
	res := &workspace.Resolution{Tip: c.GetTip(), Commit: c.GetCommit()}
	for _, f := range c.GetFiles() {
		p := string(f.GetPath())
		target, err := conflictPath(dir, conflictsFilesDir, p)
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
