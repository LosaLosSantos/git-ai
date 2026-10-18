package localrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// errMergeTreeStreamClosed is returned by Merge after Close.
var errMergeTreeStreamClosed = errors.New("merge-tree stream is closed")

// MergeTreeStream runs many merges through one long-lived
// `git merge-tree --stdin --write-tree -z --messages --allow-unrelated-histories`
// process. Each Merge writes one input line and reads exactly one record back, so
// git is started once instead of once per merge.
//
// A stream must be used by one goroutine at a time. Git reads the object database
// on every lookup, so objects written by other processes after the stream started,
// such as loose objects, are visible to later merges.
//
// Git aborts the whole process on a malformed or unmergeable input line, for example
// a missing object. Merge then returns an error and every later call returns that
// error too; Close the stream and create a new one.
//
// Unrelated histories are always accepted, as with WithAllowUnrelatedHistories,
// because git would abort the process otherwise.
//
// A git process reads the repository configuration and the gitattributes (info/attributes
// and, through attr.tree=HEAD, the .gitattributes files of HEAD's tree) once and keeps them
// for its whole lifetime, while every (*Repo).MergeTree call reads their current state. To
// return what MergeTree returns, Merge compares that state (HEAD's tree, info/attributes and
// the repository config file) with the state read before the running process was started,
// and restarts the process when it changed.
type MergeTreeStream struct {
	repo       *Repo
	repoPath   string
	objectHash git.ObjectHash
	// ctx is the context given to NewMergeTreeStream. Every process of the stream is
	// started with it.
	ctx context.Context
	// inputs is the state of the merge inputs read before the running process was started.
	inputs mergeTreeStreamInputs
	// starts counts the processes the stream started.
	starts int
	// gitOpts are the options of every process of the stream, from its HelperOptions.
	gitOpts []gitcmd.CmdOpt

	cmd    *command.Command
	reader *bufio.Reader
	stderr *bytes.Buffer
	cancel context.CancelFunc
	broken error
	closed bool
}

// mergeTreeStreamInputs is the state that a merge-tree process reads once and caches, and
// that can change while the process runs: the tree of HEAD (the attribute source, see
// gitcmd's AttrTreeConfig), info/attributes and the repository config file.
type mergeTreeStreamInputs struct {
	headTree       string
	infoAttributes string
	config         string
}

// NewMergeTreeStream starts a `git merge-tree --stdin` process for the repository.
// The process lives until Close is called or ctx is done.
func (repo *Repo) NewMergeTreeStream(ctx context.Context, opts ...HelperOption) (*MergeTreeStream, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("getting object hash %w", err)
	}

	repoPath, err := repo.Path(ctx)
	if err != nil {
		return nil, structerr.NewInternal("getting repository path: %w", err)
	}

	s := &MergeTreeStream{
		repo:       repo,
		repoPath:   repoPath,
		objectHash: objectHash,
		ctx:        ctx,
		gitOpts:    resolveHelperOptions(opts),
	}

	// The inputs are read before the process starts: a change after this point is either
	// seen by the process already or detected by the next Merge.
	inputs, err := s.readInputs(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.start(inputs); err != nil {
		return nil, err
	}

	return s, nil
}

// readInputs reads the current state of the inputs a merge-tree process caches.
func (s *MergeTreeStream) readInputs(ctx context.Context) (mergeTreeStreamInputs, error) {
	var inputs mergeTreeStreamInputs

	info, err := s.repo.ReadObjectInfo(ctx, git.Revision("HEAD^{tree}"))
	switch {
	case err == nil:
		inputs.headTree = info.Oid.String()
	case errors.As(err, new(InvalidObjectError)):
		// An unborn HEAD provides no attributes.
	default:
		return mergeTreeStreamInputs{}, structerr.NewInternal("merge-tree stream: reading HEAD tree: %w", err)
	}

	for _, file := range []struct {
		path string
		dst  *string
	}{
		{path: filepath.Join(s.repoPath, "info", "attributes"), dst: &inputs.infoAttributes},
		{path: filepath.Join(s.repoPath, "config"), dst: &inputs.config},
	} {
		content, err := os.ReadFile(file.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return mergeTreeStreamInputs{}, structerr.NewInternal("merge-tree stream: reading %q: %w", file.path, err)
		}
		*file.dst = string(content)
	}

	return inputs, nil
}

// start starts a merge-tree process that is expected to read the given inputs.
func (s *MergeTreeStream) start(inputs mergeTreeStreamInputs) error {
	ctx, cancel := context.WithCancel(s.ctx)

	stderr := &bytes.Buffer{}
	cmd, err := s.repo.Exec(ctx,
		gitcmd.Command{
			Name: "merge-tree",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--write-tree"},
				gitcmd.Flag{Name: "--stdin"},
				gitcmd.Flag{Name: "--messages"},
				gitcmd.Flag{Name: "--allow-unrelated-histories"},
			},
		},
		append([]gitcmd.CmdOpt{
			gitcmd.WithSetupStdin(),
			gitcmd.WithSetupStdout(),
			gitcmd.WithStderr(stderr),
		}, s.gitOpts...)...,
	)
	if err != nil {
		cancel()
		return err
	}

	s.inputs = inputs
	s.starts++
	s.cmd = cmd
	s.reader = bufio.NewReader(cmd)
	s.stderr = stderr
	s.cancel = cancel

	return nil
}

// stop kills the running process and reaps it.
func (s *MergeTreeStream) stop() {
	s.cancel()
	// The process was killed on purpose, so its exit status carries no information.
	_ = s.cmd.Wait()
}

// Merge merges theirs into ours. When base is not empty it is used as the merge
// base; otherwise git computes the merge base. A clean merge returns the resulting
// tree. A conflicting merge returns the tree together with a
// *MergeTreeConflictError, exactly like (*Repo).MergeTree does for the same inputs.
func (s *MergeTreeStream) Merge(ctx context.Context, base, ours, theirs git.ObjectID) (git.ObjectID, error) {
	if s.closed {
		return "", errMergeTreeStreamClosed
	}
	if s.broken != nil {
		return "", s.broken
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	for _, oid := range []git.ObjectID{base, ours, theirs} {
		if oid == "" {
			continue
		}
		if _, err := s.objectHash.FromHex(oid.String()); err != nil {
			return "", structerr.NewInvalidArgument("invalid object ID %q: %w", oid, err)
		}
	}
	if ours == "" || theirs == "" {
		return "", structerr.NewInvalidArgument("ours and theirs are required")
	}

	// A process keeps the attributes and the configuration it read first. Restart it when
	// they changed, so that the merge uses the same state a MergeTree call would read now.
	inputs, err := s.readInputs(ctx)
	if err != nil {
		return "", err
	}
	if inputs != s.inputs {
		s.stop()
		if err := s.start(inputs); err != nil {
			s.broken = structerr.NewInternal("merge-tree stream: restarting: %w", err)
			return "", s.broken
		}
	}

	line := ours.String() + " " + theirs.String() + "\n"
	if base != "" {
		line = base.String() + " -- " + line
	}

	if _, err := s.cmd.Write([]byte(line)); err != nil {
		return "", s.fail(err)
	}

	oid, mergeErr, streamErr := s.readRecord()
	if streamErr != nil {
		return "", s.fail(streamErr)
	}

	return oid, mergeErr
}

// Close kills the merge-tree process and reaps it. Close is idempotent.
func (s *MergeTreeStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true

	s.stop()

	return nil
}

// fail kills and reaps the process, marks the stream as broken, and returns the
// error that Merge reports.
func (s *MergeTreeStream) fail(err error) error {
	s.stop()

	s.broken = structerr.NewInternal("merge-tree stream: %w", err).
		WithMetadata("stderr", s.stderr.String())

	return s.broken
}

// readRecord reads one record of `git merge-tree --stdin -z --messages` output. The
// record is:
//
//	<clean flag> NUL <tree> NUL { <conflicted file info> NUL }* NUL
//	{ <count> NUL { <path> NUL }{count} <type> NUL <message> NUL }* NUL
//
// Conflicted file info and messages are only present for conflicts and messages.
// The part after the clean flag is rebuilt as the output of a non-stdin
// merge-tree call, so parseMergeTreeError can parse it exactly like MergeTree does.
//
// mergeErr is the merge result, or the conflict error. streamErr is set when the
// output could not be read and the stream cannot be used anymore.
func (s *MergeTreeStream) readRecord() (oid git.ObjectID, mergeErr error, streamErr error) {
	clean, err := s.readField()
	if err != nil {
		return "", nil, err
	}
	if clean != "0" && clean != "1" {
		return "", nil, fmt.Errorf("unexpected clean flag %q", clean)
	}

	tree, err := s.readField()
	if err != nil {
		return "", nil, err
	}

	var body strings.Builder
	body.WriteString(tree)
	body.WriteByte(0)

	// Conflicted file info lines end with an empty field.
	for {
		field, err := s.readField()
		if err != nil {
			return "", nil, err
		}
		if field == "" {
			break
		}
		body.WriteString(field)
		body.WriteByte(0)
	}
	body.WriteByte(0)

	// Message groups end with an empty field.
	for {
		field, err := s.readField()
		if err != nil {
			return "", nil, err
		}
		if field == "" {
			break
		}
		body.WriteString(field)
		body.WriteByte(0)

		count, err := strconv.Atoi(field)
		if err != nil {
			return "", nil, fmt.Errorf("parsing message path count: %w", err)
		}

		// The paths, the conflict type and the message follow the count.
		for i := 0; i < count+2; i++ {
			field, err := s.readField()
			if err != nil {
				return "", nil, err
			}
			body.WriteString(field)
			body.WriteByte(0)
		}
	}

	if clean == "1" {
		oid, err := s.objectHash.FromHex(tree)
		if err != nil {
			return "", structerr.NewInternal("hex to oid: %w", err), nil
		}
		return oid, nil, nil
	}

	oid, err = parseMergeTreeError(s.objectHash, mergeTreeConfig{}, body.String())
	return oid, err, nil
}

// readField reads the next NUL-terminated field of the process output.
func (s *MergeTreeStream) readField() (string, error) {
	field, err := s.reader.ReadString(0)
	if err != nil {
		return "", err
	}

	return field[:len(field)-1], nil
}

// HelperOption configures the long-lived git processes of a MergeTreeStream or a CommitWriter.
type HelperOption func(*helperConfig)

type helperConfig struct {
	gitOpts []gitcmd.CmdOpt
}

// WithoutObjectFsync starts the helper processes with core.fsync=none. A MergeTreeStream and a
// CommitWriter write nothing but objects, so the option only drops the fsync of the objects they
// write. The caller must make those objects durable itself, before a quarantine that holds them is
// migrated: every object file in a quarantine must be durable at migration time, and the migration
// is not relied on to fsync them. The batch attempts of UserLand pack every loose object of their
// quarantine into one fsynced pack before the update (see doc/graft/PLAN8.md, section 3.2).
//
// The value replaces the core.fsync of the server for the helper. It cannot be derived from it: git
// does not subtract a component from a value set earlier (a later "-loose-object" replaces the
// whole value), and the helpers write no reference, index or derived metadata that the other
// components would cover.
func WithoutObjectFsync() HelperOption {
	return func(cfg *helperConfig) {
		cfg.gitOpts = append(cfg.gitOpts, gitcmd.WithConfig(gitcmd.ConfigPair{Key: "core.fsync", Value: "none"}))
	}
}

// resolveHelperOptions returns the command options of the helper processes configured by opts.
func resolveHelperOptions(opts []HelperOption) []gitcmd.CmdOpt {
	var cfg helperConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg.gitOpts
}
