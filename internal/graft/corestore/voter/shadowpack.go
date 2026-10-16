package voter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// buildPackAgainst writes the pack of the objects reachable from tips and not
// from any object ID among known, the values of the references that the
// database records for the repository. It is the pack of a shadow-mode
// changeset.
//
// The pack of authoritative mode (gitops.BuildPack) leaves out what any local
// reference reaches, which is right when every local reference value went
// through the database first (invariant I1: every object reachable from a
// local reference is in an earlier pack). Shadow mode breaks that invariant:
// writes that the storage core did not record (while an import is not
// complete, after a shadow error or timeout, or through an RPC that bypasses
// the hook) move local references to objects that no pack holds. Packs are
// never deleted, so every object reachable from a value the database recorded
// at any time is in the bucket; excluding exactly those values keeps the
// bucket complete for every value the database records next.
//
// Known object IDs that the repository does not have (a value the database
// recorded and git no longer has) are left out of the exclusion: excluding
// fewer objects only makes the pack larger. A tip that the repository does not
// have is an error.
func buildPackAgainst(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID, known map[string]corestore.Target, tmpDir string) (_ gitops.Pack, returnedErr error) {
	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return gitops.Pack{}, fmt.Errorf("object hash: %w", err)
	}

	positive := map[string]struct{}{}
	for _, tip := range tips {
		if hash.IsZeroOID(tip) {
			continue
		}
		if err := hash.ValidateHex(tip.String()); err != nil {
			return gitops.Pack{}, fmt.Errorf("tip: %w", err)
		}
		positive[tip.String()] = struct{}{}
	}
	if len(positive) == 0 {
		return gitops.Pack{}, nil
	}

	// The database values are untrusted input (RULE 10): only valid object IDs
	// of the repository's hash reach git, and only on its standard input.
	candidates := map[string]struct{}{}
	for name, target := range known {
		if target.Absent() || target.IsSymbolic() {
			continue
		}
		if err := hash.ValidateHex(string(target)); err != nil {
			return gitops.Pack{}, fmt.Errorf("database value of %q: %w", name, err)
		}
		candidates[string(target)] = struct{}{}
	}

	excluded, err := existingObjects(ctx, repo, candidates)
	if err != nil {
		return gitops.Pack{}, err
	}

	var stdin bytes.Buffer
	for _, oid := range sortedKeys(positive) {
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}
	for _, oid := range excluded {
		stdin.WriteByte('^')
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}

	file, err := os.CreateTemp(tmpDir, "graft-pack-*")
	if err != nil {
		return gitops.Pack{}, fmt.Errorf("create pack file: %w", err)
	}
	keep := false
	defer func() {
		if returnedErr != nil || !keep {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()

	// pack-objects --revs reads the revisions like rev-list --objects: the tips,
	// then the excluded values with a ^ prefix. The pack is not thin.
	hasher := sha256.New()
	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name: "pack-objects",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "-q"},
			gitcmd.Flag{Name: "--revs"},
			gitcmd.Flag{Name: "--stdout"},
		},
	},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(io.MultiWriter(file, hasher)),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return gitops.Pack{}, structerr.New("pack-objects: %w", err).WithMetadata("stderr", stderr.String())
	}

	info, err := file.Stat()
	if err != nil {
		return gitops.Pack{}, fmt.Errorf("stat pack file: %w", err)
	}
	var header [12]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return gitops.Pack{}, fmt.Errorf("read pack header: %w", err)
	}
	if string(header[:4]) != "PACK" {
		return gitops.Pack{}, errors.New("pack-objects wrote no pack header")
	}

	objects := int(binary.BigEndian.Uint32(header[8:12]))
	if objects == 0 {
		// The transaction brings nothing new: no pack, like gitops.BuildPack.
		return gitops.Pack{}, nil
	}

	if err := file.Close(); err != nil {
		return gitops.Pack{}, fmt.Errorf("close pack file: %w", err)
	}
	keep = true

	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))
	return gitops.Pack{
		Path:    file.Name(),
		SHA256:  sum,
		Size:    info.Size(),
		Objects: objects,
	}, nil
}

// existingObjects returns the object IDs among oids (validated hex object IDs)
// that the repository has, sorted, with one cat-file run.
func existingObjects(ctx context.Context, repo *localrepo.Repo, oids map[string]struct{}) ([]string, error) {
	if len(oids) == 0 {
		return nil, nil
	}

	var stdin bytes.Buffer
	for _, oid := range sortedKeys(oids) {
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}

	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "cat-file",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--batch-check=%(objectname)"},
				gitcmd.Flag{Name: "--buffer"},
			},
		},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, structerr.New("cat-file: %w", err).WithMetadata("stderr", stderr.String())
	}

	// A present object is printed as its object ID, a missing one as
	// "<oid> missing".
	var existing []string
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		oid := scanner.Text()
		if strings.HasSuffix(oid, " missing") {
			continue
		}
		if _, ok := oids[oid]; !ok {
			return nil, fmt.Errorf("cat-file: unexpected output %q", oid)
		}
		existing = append(existing, oid)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cat-file: %w", err)
	}

	sort.Strings(existing)
	return existing, nil
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
