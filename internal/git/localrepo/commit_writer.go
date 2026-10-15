package localrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/helper/text"
)

// ErrCommitWriterUnsupported is returned by CommitWriter.Write for commits that must be written by
// WriteCommit instead. These are commits that are to be signed, commits that are written into an
// alternate object directory, commits whose object contents are not valid UTF-8 or contain NUL
// bytes, carriage returns or Unicode noncharacters, commits whose names or emails git would
// change or reject, commits in a repository with a non-UTF-8 i18n.commitEncoding, commits with
// timestamps or timezone offsets that the writer does not reproduce, and commits whose tree or
// parents are missing or have the wrong object type. Git rewrites or rejects such commits while
// committing, which the commit writer does not reproduce.
var ErrCommitWriterUnsupported = errors.New("commit writer does not support this commit")

// errCommitWriterClosed is returned by CommitWriter.Write after Close has been called.
var errCommitWriterClosed = errors.New("commit writer is closed")

// maxCommitWriterAttempts is how many hash-object processes a single Write may use. A Write whose
// process dies starts a new process and tries once more, which is safe because hashing a file is
// idempotent.
const maxCommitWriterAttempts = 2

// CommitWriter writes commit objects through one long-lived
// `git hash-object -t commit -w --literally --no-filters --stdin-paths` process. The commit object is
// formatted in Go exactly like `git commit-tree` formats it, and the formatted object is written to a
// temporary file whose path is sent to the process. The temporary file is removed after its object
// ID has been read.
//
// If the process dies or its output cannot be parsed, the process is killed and a new one is started
// for the next attempt, so a failed Write leaves the writer usable.
//
// A CommitWriter must only be used by a single goroutine at a time. Close must be called to end the
// process once the writer is no longer needed.
type CommitWriter struct {
	repo       *Repo
	objectHash git.ObjectHash
	tempDir    string

	// ctx is the context the writer was created with. Every hash-object process runs under it,
	// including processes that replace a process that died, so that all of them end with it.
	ctx context.Context

	// process is the running hash-object process, or nil if it must be started before the next
	// write because the previous one failed.
	process *commitWriterProcess

	// encodingUnsupported is set when i18n.commitEncoding names an encoding other than UTF-8.
	// commit-tree then writes an encoding header that the writer does not reproduce. It is read
	// whenever a hash-object process is started.
	encodingUnsupported bool

	// closed is set by the first Close, whose result is kept in closeErr.
	closed   bool
	closeErr error
}

// commitWriterProcess is one `git hash-object` process of a CommitWriter.
type commitWriterProcess struct {
	cmd    *command.Command
	stdout *bufio.Reader
	// cancel cancels the context of the process, which kills and reaps the process.
	cancel context.CancelFunc
}

// NewCommitWriter starts the long-lived process used to write commit objects. The process, and any
// process that replaces it, is terminated when ctx is done or when Close is called.
func (repo *Repo) NewCommitWriter(ctx context.Context) (*CommitWriter, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	tempDir, err := repo.StorageTempDir()
	if err != nil {
		return nil, fmt.Errorf("getting temporary directory: %w", err)
	}

	writer := &CommitWriter{
		repo:       repo,
		objectHash: objectHash,
		tempDir:    tempDir,
		ctx:        ctx,
	}

	if err := writer.startProcess(); err != nil {
		return nil, err
	}

	return writer, nil
}

// startProcess reads i18n.commitEncoding and starts a new hash-object process under the context of
// the writer.
func (w *CommitWriter) startProcess() error {
	if err := w.ctx.Err(); err != nil {
		return fmt.Errorf("starting hash-object: %w", err)
	}

	encoding, encodingSet, err := readCommitEncoding(w.ctx, w.repo)
	if err != nil {
		return fmt.Errorf("reading i18n.commitEncoding: %w", err)
	}

	processCtx, cancel := context.WithCancel(w.ctx)

	cmd, err := w.repo.Exec(processCtx,
		gitcmd.Command{
			Name: "hash-object",
			Flags: []gitcmd.Option{
				gitcmd.ValueFlag{Name: "-t", Value: "commit"},
				gitcmd.Flag{Name: "-w"},
				gitcmd.Flag{Name: "--literally"},
				gitcmd.Flag{Name: "--no-filters"},
				gitcmd.Flag{Name: "--stdin-paths"},
			},
		},
		gitcmd.WithSetupStdin(),
		gitcmd.WithSetupStdout(),
	)
	if err != nil {
		cancel()
		return fmt.Errorf("starting hash-object: %w", err)
	}

	w.encodingUnsupported = encodingSet && !isUTF8CommitEncoding(encoding)
	w.process = &commitWriterProcess{
		cmd:    cmd,
		stdout: bufio.NewReader(cmd),
		cancel: cancel,
	}

	return nil
}

// stopProcess kills and reaps the current hash-object process, if any. The next write starts a new
// one.
func (w *CommitWriter) stopProcess() {
	if w.process == nil {
		return
	}

	w.process.cancel()
	// The process was killed, so its exit status carries no information.
	_ = w.process.cmd.Wait()
	w.process = nil
}

// readCommitEncoding returns the value of i18n.commitEncoding as git reads it from all configuration
// scopes, and whether the option is set.
func readCommitEncoding(ctx context.Context, repo *Repo) (string, bool, error) {
	var stdout bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "config",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--get"}},
			Args:  []string{"i18n.commitEncoding"},
		},
		gitcmd.WithStdout(&stdout),
	); err != nil {
		if isExitWithCode(err, 1) {
			return "", false, nil
		}

		return "", false, err
	}

	return text.ChompBytes(stdout.Bytes()), true, nil
}

// isUTF8CommitEncoding reports whether git treats the encoding name as UTF-8, mirroring git's
// same_utf_encoding: "utf" in any case, then at most one '-', then "8". For UTF-8, commit-tree writes
// no encoding header. Names that are not recognised as UTF-8 are treated as other encodings, which is
// the conservative choice.
func isUTF8CommitEncoding(name string) bool {
	normalized := strings.ToLower(name)
	rest, ok := strings.CutPrefix(normalized, "utf")
	if !ok {
		return false
	}

	rest = strings.TrimPrefix(rest, "-")
	return rest == "8"
}

// Write writes the commit described by cfg and returns its object ID. The object ID is the same
// as the one WriteCommit returns for the same configuration. Configurations that WriteCommit
// would sign, that write into an alternate object directory, or whose object git would write
// differently or reject return ErrCommitWriterUnsupported before anything is written; callers must
// then fall back to WriteCommit.
//
// Like WriteCommit, Write updates cfg.Reference to the new commit when it is set.
//
// Write checks that the tree and the parents exist with the object types commit-tree requires.
// A Write that fails, for example because ctx is canceled or the hash-object process died, leaves
// the writer usable for the next Write.
func (w *CommitWriter) Write(ctx context.Context, cfg WriteCommitConfig) (git.ObjectID, error) {
	if w.closed {
		return "", errCommitWriterClosed
	}

	if err := validateWriteCommitConfig(cfg); err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	if cfg.AlternateObjectDir != "" {
		return "", ErrCommitWriterUnsupported
	}

	if featureflag.GPGSigning.IsEnabled(ctx) && cfg.Sign && cfg.GitConfig.SigningKey != "" {
		return "", ErrCommitWriterUnsupported
	}

	if cfg.AuthorDate.IsZero() {
		cfg.AuthorDate = time.Now()
	}

	if cfg.CommitterDate.IsZero() {
		cfg.CommitterDate = time.Now()
	}

	if !isWritableSignatureTime(cfg.AuthorDate) || !isWritableSignatureTime(cfg.CommitterDate) {
		return "", ErrCommitWriterUnsupported
	}

	committerName, committerEmail := cfg.CommitterName, cfg.CommitterEmail
	if featureflag.GPGSigning.IsEnabled(ctx) && cfg.Sign && cfg.GitConfig.CommitterName != "" && cfg.GitConfig.CommitterEmail != "" {
		committerName, committerEmail = cfg.GitConfig.CommitterName, cfg.GitConfig.CommitterEmail
	}

	object, err := w.formatCommit(cfg, committerName, committerEmail)
	if err != nil {
		return "", err
	}

	// The process is started here when an earlier failure stopped it. Starting it also reads
	// i18n.commitEncoding.
	if w.process == nil {
		if err := w.startProcess(); err != nil {
			return "", err
		}
	}

	if w.encodingUnsupported {
		return "", ErrCommitWriterUnsupported
	}

	if err := w.checkObjects(ctx, cfg); err != nil {
		return "", err
	}

	oid, err := w.writeObject(ctx, object)
	if err != nil {
		return "", err
	}

	if cfg.Reference != "" {
		if err := w.repo.UpdateRef(
			ctx,
			git.ReferenceName(cfg.Reference),
			oid,
			"",
		); err != nil {
			return "", fmt.Errorf("updating ref: %w", err)
		}
	}

	return oid, nil
}

// maxSignatureOffset is the largest timezone offset that git's date parser accepts. git's match_tz
// ignores offsets whose hours are 24 or more and uses the local timezone instead, which changes both
// the timestamp and the offset that commit-tree writes.
const maxSignatureOffset = 24*time.Hour - time.Minute

// isWritableSignatureTime reports whether the commit writer reproduces the timestamp that WriteCommit
// writes for when. WriteCommit passes the time to commit-tree as an RFC 2822 date, which drops the
// seconds of an offset, and git ignores offsets of 24 hours or more. Git parses the local date of
// that string and rejects local years before 1970 or after 2099, so the year is checked in the
// time's own zone as well as in UTC. Git's fsck also rejects dates before the Unix epoch.
func isWritableSignatureTime(when time.Time) bool {
	if when.Unix() < 0 || when.UTC().Year() > 2099 {
		return false
	}

	if year := when.Year(); year < 1970 || year > 2099 {
		return false
	}

	_, offset := when.Zone()
	if offset%60 != 0 {
		return false
	}

	offsetDuration := time.Duration(offset) * time.Second
	return -maxSignatureOffset <= offsetDuration && offsetDuration <= maxSignatureOffset
}

// checkObjects returns ErrCommitWriterUnsupported unless the tree is a tree and every parent is a
// commit that exists in the repository. commit-tree rejects such commits, so WriteCommit must write
// them and report the error.
func (w *CommitWriter) checkObjects(ctx context.Context, cfg WriteCommitConfig) error {
	if err := w.checkObjectType(ctx, cfg.TreeID, "tree"); err != nil {
		return err
	}

	for _, parent := range cfg.Parents {
		if err := w.checkObjectType(ctx, parent, "commit"); err != nil {
			return err
		}
	}

	return nil
}

// checkObjectType returns ErrCommitWriterUnsupported if oid does not exist or is not of the wanted type.
func (w *CommitWriter) checkObjectType(ctx context.Context, oid git.ObjectID, wantType string) error {
	info, err := w.repo.ReadObjectInfo(ctx, oid.Revision())
	if err != nil {
		var invalid InvalidObjectError
		if errors.As(err, &invalid) {
			return ErrCommitWriterUnsupported
		}

		return fmt.Errorf("reading object %s: %w", oid, err)
	}

	if info.Type != wantType {
		return ErrCommitWriterUnsupported
	}

	return nil
}

// formatCommit returns the raw commit object exactly as `git commit-tree` writes it for the given
// configuration without signing. The committer is passed separately because WriteCommit may swap
// it for the configured committer. It returns ErrCommitWriterUnsupported for any configuration whose
// object commit-tree would write differently or reject.
func (w *CommitWriter) formatCommit(cfg WriteCommitConfig, committerName, committerEmail string) ([]byte, error) {
	var buf bytes.Buffer

	// commit-tree accepts IDs that FromHex rejects, such as uppercase or abbreviated IDs, and writes
	// the full lowercase ID. Those are left to WriteCommit.
	tree, err := w.objectHash.FromHex(string(cfg.TreeID))
	if err != nil {
		return nil, ErrCommitWriterUnsupported
	}
	fmt.Fprintf(&buf, "tree %s\n", tree)

	// git commit-tree drops duplicate parents, keeping the first occurrence.
	seenParents := make(map[git.ObjectID]struct{}, len(cfg.Parents))
	for _, parent := range cfg.Parents {
		parentOID, err := w.objectHash.FromHex(parent.String())
		if err != nil {
			return nil, ErrCommitWriterUnsupported
		}

		if _, ok := seenParents[parentOID]; ok {
			continue
		}
		seenParents[parentOID] = struct{}{}

		fmt.Fprintf(&buf, "parent %s\n", parentOID)
	}

	author, err := formatIdent(cfg.AuthorName, cfg.AuthorEmail, cfg.AuthorDate)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&buf, "author %s\n", author)

	committer, err := formatIdent(committerName, committerEmail, cfg.CommitterDate)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&buf, "committer %s\n", committer)

	buf.WriteByte('\n')
	buf.WriteString(cfg.Message)

	object := buf.Bytes()
	if bytes.IndexByte(object, 0) >= 0 ||
		bytes.IndexByte(object, '\r') >= 0 ||
		!utf8.Valid(object) ||
		containsUTF8Noncharacter(object) {
		return nil, ErrCommitWriterUnsupported
	}

	return object, nil
}

// containsUTF8Noncharacter reports whether the valid UTF-8 in object contains a noncharacter. git's
// verify_utf8 (find_invalid_utf8) treats U+xxFFFE and U+xxFFFF, and U+FDD0 to U+FDEF, as invalid and
// re-encodes the bytes of the whole commit object as Latin-1. The writer must not store those bytes
// as they are, so the object must be left to WriteCommit. object must already be valid UTF-8.
func containsUTF8Noncharacter(object []byte) bool {
	for len(object) > 0 {
		r, size := utf8.DecodeRune(object)
		if r&0xfffe == 0xfffe || (r >= 0xfdd0 && r <= 0xfdef) {
			return true
		}
		object = object[size:]
	}

	return false
}

// writeObject writes the raw commit object into the repository through the hash-object process
// and returns its object ID. If the process fails, it is killed and a new process is started for
// one more attempt. If ctx is done while the process is in use, the process is killed and the
// context error is returned.
func (w *CommitWriter) writeObject(ctx context.Context, object []byte) (_ git.ObjectID, returnedErr error) {
	file, err := os.CreateTemp(w.tempDir, "commit-object-*")
	if err != nil {
		return "", fmt.Errorf("creating commit object file: %w", err)
	}

	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) && returnedErr == nil {
			returnedErr = fmt.Errorf("removing commit object file: %w", err)
		}
	}()

	if _, err := file.Write(object); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("writing commit object file: %w", err)
	}

	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing commit object file: %w", err)
	}

	for attempt := 1; ; attempt++ {
		if w.process == nil {
			if err := w.startProcess(); err != nil {
				return "", err
			}

			// The new process may see a different i18n.commitEncoding.
			if w.encodingUnsupported {
				return "", ErrCommitWriterUnsupported
			}
		}

		oid, err := w.hashFile(ctx, file.Name())
		if err == nil {
			return oid, nil
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("writing commit object: %w", ctxErr)
		}

		if attempt >= maxCommitWriterAttempts {
			return "", err
		}
	}
}

// hashFile sends path to the hash-object process and reads the object ID it prints. The process is
// killed if ctx is done before the object ID has been read. On any error, the process is stopped so
// that the next attempt starts a new one.
func (w *CommitWriter) hashFile(ctx context.Context, path string) (git.ObjectID, error) {
	process := w.process

	stopKilling := context.AfterFunc(ctx, process.cancel)
	oid, err := process.hash(w.objectHash, path)
	if !stopKilling() {
		// ctx is done and the process was or is being killed, even if the object ID was read.
		w.stopProcess()
		if err == nil {
			err = fmt.Errorf("writing commit object: %w", ctx.Err())
		}
		return "", err
	}

	if err != nil {
		w.stopProcess()
		return "", err
	}

	return oid, nil
}

// hash sends path to the process and reads the object ID it prints.
func (p *commitWriterProcess) hash(objectHash git.ObjectHash, path string) (git.ObjectID, error) {
	if _, err := fmt.Fprintf(p.cmd.Stdin(), "%s\n", path); err != nil {
		return "", fmt.Errorf("sending commit object path: %w", err)
	}

	line, err := p.stdout.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading commit object ID: %w", err)
	}

	oid, err := objectHash.FromHex(strings.TrimSuffix(line, "\n"))
	if err != nil {
		return "", fmt.Errorf("parsing commit object ID: %w", err)
	}

	return oid, nil
}

// Close ends the hash-object process and waits for it to exit. Close is idempotent: later calls
// return the result of the first one. Stdin that is already closed, for example because the context
// was canceled and the process was reaped, is not an error. Write fails after Close.
func (w *CommitWriter) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true

	process := w.process
	if process == nil {
		return nil
	}
	w.process = nil
	defer process.cancel()

	if err := process.cmd.Stdin().Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		_ = process.cmd.Wait()
		w.closeErr = fmt.Errorf("closing commit writer stdin: %w", err)
		return w.closeErr
	}

	if err := process.cmd.Wait(); err != nil {
		w.closeErr = fmt.Errorf("waiting for commit writer: %w", err)
	}

	return w.closeErr
}

// formatIdent formats an author or committer line. git's fmt_ident removes crud from the ends of the
// name and the email and removes newlines and angle brackets from them, and which characters count
// as crud depends on the git version. The writer only writes identities that every git version
// writes unchanged, and returns ErrCommitWriterUnsupported for all others, including names that git
// rejects because they consist only of disallowed characters. Empty emails are left to WriteCommit
// as well.
func formatIdent(name, email string, when time.Time) (string, error) {
	if email == "" || !isVerbatimIdentPart(name) || !isVerbatimIdentPart(email) {
		return "", ErrCommitWriterUnsupported
	}

	return fmt.Sprintf("%s <%s> %s", name, email, git.FormatSignatureTime(when)), nil
}

// isVerbatimIdentPart reports whether git's strbuf_addstr_without_crud copies value unchanged with
// any git version: value is not empty, does not start or end with crud, and contains no newline or
// angle bracket.
func isVerbatimIdentPart(value string) bool {
	if value == "" || isIdentCrud(value[0]) || isIdentCrud(value[len(value)-1]) {
		return false
	}

	return !strings.ContainsAny(value, "\n<>")
}

// isIdentCrud mirrors git's crud(): control characters, space and the punctuation that git strips
// from the ends of identities. Git 2.53 stopped treating dots as crud, so they are included to stay
// conservative with every git version.
func isIdentCrud(c byte) bool {
	return c <= ' ' || strings.IndexByte(".,:;<>\"\\'", c) >= 0
}
