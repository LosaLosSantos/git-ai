// Package rebase implements the rebase used by graft batch landing with long-lived git helper
// processes. It produces the same results as (*localrepo.Repo).Rebase, without starting several
// git processes per rebased commit.
package rebase

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// mergeStream merges trees. It is implemented by *localrepo.MergeTreeStream.
type mergeStream interface {
	// Merge merges theirs into ours with base as the merge base ("" lets git compute it). A
	// conflicting merge returns *localrepo.MergeTreeConflictError.
	Merge(ctx context.Context, base, ours, theirs git.ObjectID) (git.ObjectID, error)
	Close() error
}

// commitWriter writes commit objects. It is implemented by *localrepo.CommitWriter.
type commitWriter interface {
	// Write returns localrepo.ErrCommitWriterUnsupported when the commit cannot be written by the
	// writer, in which case the commit is written with (*localrepo.Repo).WriteCommit.
	Write(ctx context.Context, cfg localrepo.WriteCommitConfig) (git.ObjectID, error)
	Close() error
}

// backends starts the helper processes of an Engine lazily.
type backends struct {
	newMergeStream  func(ctx context.Context) (mergeStream, error)
	newCommitWriter func(ctx context.Context) (commitWriter, error)
}

// Engine rebases commits within one batch attempt using one merge-tree stream and one commit
// writer, started lazily and closed by Close. An Engine is not safe for concurrent use.
//
// The helper processes are started with the context of the call that needs them and die with
// it. A helper whose context is done is replaced by a new one on the next use, so a canceled
// context of one Rebase does not fail the later Rebase calls of the Engine. A helper that
// fails with an error other than a merge conflict is replaced in the same way, so that one
// failed rebase does not fail the rebases that follow it.
type Engine struct {
	repo     *localrepo.Repo
	backends backends

	stream mergeStream
	// streamDone is the Done channel of the context the stream was started with.
	streamDone <-chan struct{}

	writer commitWriter
	// writerDone is the Done channel of the context the writer was started with.
	writerDone <-chan struct{}
}

// New returns an Engine for repo.
func New(repo *localrepo.Repo) *Engine {
	return newEngine(repo, backends{
		newMergeStream: func(ctx context.Context) (mergeStream, error) {
			stream, err := repo.NewMergeTreeStream(ctx)
			if err != nil {
				return nil, err
			}
			return stream, nil
		},
		newCommitWriter: func(ctx context.Context) (commitWriter, error) {
			writer, err := repo.NewCommitWriter(ctx)
			if err != nil {
				return nil, err
			}
			return writer, nil
		},
	})
}

func newEngine(repo *localrepo.Repo, b backends) *Engine {
	return &Engine{repo: repo, backends: b}
}

// Rebase has the semantics and errors of (*localrepo.Repo).Rebase(ctx, upstream, branch, opts...).
func (e *Engine) Rebase(ctx context.Context, upstream, branch git.ObjectID, opts ...localrepo.RebaseOption) (git.ObjectID, error) {
	settings := localrepo.ResolveRebaseOptions(opts...)

	upstreamOID, err := e.repo.ResolveRevision(ctx, git.Revision(upstream.String()+"^{commit}"))
	if err != nil {
		return "", structerr.NewInvalidArgument("resolving upstream commit: %w", err).WithMetadata("revision", upstream.String())
	}

	branchOID, err := e.repo.ResolveRevision(ctx, git.Revision(branch.String()+"^{commit}"))
	if err != nil {
		return "", structerr.NewInvalidArgument("resolving branch commit: %w", err).WithMetadata("revision", branch.String())
	}

	var stdout bytes.Buffer
	if err := e.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "merge-base",
			Args: []string{upstreamOID.String(), branchOID.String()},
		},
		gitcmd.WithStdout(&stdout),
	); err != nil {
		return "", structerr.NewInternal("get merge-base: %w", err)
	}

	// fast-forward if possible
	mergeBase := text.ChompBytes(stdout.Bytes())
	if mergeBase == upstreamOID.String() {
		return branchOID, nil
	}
	if mergeBase == branchOID.String() {
		return upstreamOID, nil
	}

	newOID, err := e.rebaseCommits(ctx, settings, upstreamOID, branchOID)
	if err != nil {
		return "", structerr.NewInternal("rebase using merge-tree: %w", err)
	}

	return newOID, nil
}

// Close ends the helper processes started by the Engine. It is safe to call more than once. A
// helper that was already ended by the cancellation of its context is not an error.
func (e *Engine) Close() error {
	var errs []error
	if e.stream != nil {
		errs = append(errs, closeHelper(e.streamDone, e.stream.Close))
		e.stream, e.streamDone = nil, nil
	}
	if e.writer != nil {
		errs = append(errs, closeHelper(e.writerDone, e.writer.Close))
		e.writer, e.writerDone = nil, nil
	}
	return errors.Join(errs...)
}

// closeHelper closes a helper process. The error of a helper whose context is done is dropped,
// because the process was killed by the cancellation.
func closeHelper(done <-chan struct{}, closeFn func() error) error {
	err := closeFn()
	if isDone(done) {
		return nil
	}
	return err
}

// isDone reports whether the channel is closed. A nil channel, the Done channel of a context that
// can never be canceled, is never done.
func isDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (e *Engine) rebaseCommits(ctx context.Context, settings localrepo.RebaseSettings, upstreamOID, branchOID git.ObjectID) (git.ObjectID, error) {
	objectHash, err := e.repo.ObjectHash(ctx)
	if err != nil {
		return "", structerr.NewInternal("getting object hash %w", err)
	}

	todoList, err := e.todoList(ctx, settings, upstreamOID, branchOID)
	if err != nil {
		return "", err
	}

	upstreamCommit, err := e.repo.ReadCommit(ctx, git.Revision(upstreamOID))
	if err != nil {
		return "", fmt.Errorf("reading upstream commit: %w", err)
	}

	oursCommitOID := upstreamOID
	oursTreeOID := git.ObjectID(upstreamCommit.GetTreeId())
	for _, todoItem := range todoList {
		theirsCommit, err := e.repo.ReadCommit(ctx, git.Revision(todoItem))
		if err != nil {
			return "", fmt.Errorf("reading todo list commit: %w", err)
		}

		var base git.ObjectID
		if len(theirsCommit.GetParentIds()) > 0 {
			base = git.ObjectID(theirsCommit.GetParentIds()[0])
		}

		newTreeOID, err := e.merge(ctx, base, oursCommitOID, git.ObjectID(theirsCommit.GetId()))
		if err != nil {
			var conflictErr *localrepo.MergeTreeConflictError
			if errors.As(err, &conflictErr) {
				return newTreeOID, &localrepo.RebaseConflictError{
					Commit:        theirsCommit.GetId(),
					ConflictError: conflictErr,
				}
			}
			return "", fmt.Errorf("merging todo list commit: %w", err)
		}

		// Empty commits are handled as in (*localrepo.Repo).Rebase: a commit whose merge changes
		// nothing is picked only if it is itself empty, a root commit excepted when empty.
		if newTreeOID == oursTreeOID {
			if len(theirsCommit.GetParentIds()) == 0 {
				if theirsCommit.GetTreeId() != objectHash.EmptyTreeOID.String() {
					continue
				}
			} else {
				theirsParentCommit, err := e.repo.ReadCommit(ctx, git.Revision(theirsCommit.GetParentIds()[0]))
				if err != nil {
					return "", fmt.Errorf("reading parent commit: %w", err)
				}

				if theirsCommit.GetTreeId() != theirsParentCommit.GetTreeId() {
					continue
				}
			}
		}

		author := signatureFromCommitAuthor(theirsCommit.GetAuthor())
		committer := settings.Committer
		if committer == nil {
			committer = signatureFromCommitAuthor(theirsCommit.GetCommitter())
		}

		message, err := e.fullCommitMessage(ctx, theirsCommit)
		if err != nil {
			return "", fmt.Errorf("reading commit message: %w", err)
		}

		newCommitOID, err := e.writeCommit(ctx, localrepo.WriteCommitConfig{
			Parents:        []git.ObjectID{oursCommitOID},
			AuthorName:     author.Name,
			AuthorEmail:    author.Email,
			AuthorDate:     author.When,
			CommitterName:  committer.Name,
			CommitterEmail: committer.Email,
			CommitterDate:  committer.When,
			Message:        string(message),
			TreeID:         newTreeOID,
		})
		if err != nil {
			return "", fmt.Errorf("write commit: %w", err)
		}
		oursCommitOID = newCommitOID
		oursTreeOID = newTreeOID
	}

	return oursCommitOID, nil
}

// todoList returns the commits of branch that are not in upstream, oldest first, without the
// clean cherry-picks of upstream unless they are reapplied. It is the list that
// (*localrepo.Repo).Rebase replays.
func (e *Engine) todoList(ctx context.Context, settings localrepo.RebaseSettings, upstreamOID, branchOID git.ObjectID) ([]string, error) {
	// Flags of git-rev-list to get the pick-only todo_list for a rebase, see
	// (*localrepo.Repo).Rebase for their origin.
	var flags []gitcmd.Option
	if !settings.ReapplyCherryPicks {
		flags = append(flags, gitcmd.Flag{Name: "--cherry-pick"})
	}
	flags = append(flags,
		gitcmd.Flag{Name: "--right-only"},
		gitcmd.Flag{Name: "--no-merges"},
		gitcmd.Flag{Name: "--topo-order"},
		gitcmd.Flag{Name: "--reverse"},
	)

	var stderr bytes.Buffer
	cmd, err := e.repo.Exec(ctx, gitcmd.Command{
		Name:  "rev-list",
		Flags: flags,
		Args:  []string{fmt.Sprintf("%s...%s", upstreamOID, branchOID)},
	}, gitcmd.WithStderr(&stderr), gitcmd.WithSetupStdout())
	if err != nil {
		return nil, structerr.NewInternal("start git rev-list: %w", err)
	}

	var todoList []string
	scanner := bufio.NewScanner(cmd)
	for scanner.Scan() {
		todoList = append(todoList, strings.TrimSpace(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning rev-list output: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, structerr.NewInternal("git rev-list: %w", err).WithMetadata("stderr", stderr.String())
	}

	return todoList, nil
}

// merge merges theirs into ours with base as the merge base. base is empty for a root commit.
// A merge conflict leaves the stream usable. Any other error ends the stream, because git aborts
// the process on an unmergeable input, and the next merge starts a new stream.
func (e *Engine) merge(ctx context.Context, base, ours, theirs git.ObjectID) (git.ObjectID, error) {
	stream, err := e.getStream(ctx)
	if err != nil {
		return "", fmt.Errorf("starting merge-tree stream: %w", err)
	}

	oid, err := stream.Merge(ctx, base, ours, theirs)
	var conflictErr *localrepo.MergeTreeConflictError
	if err != nil && !errors.As(err, &conflictErr) {
		e.dropStream()
	}

	return oid, err
}

// writeCommit writes the commit with the commit writer and falls back to WriteCommit when the
// writer does not support it. Any other error ends the writer, and the next write starts a new
// one.
func (e *Engine) writeCommit(ctx context.Context, cfg localrepo.WriteCommitConfig) (git.ObjectID, error) {
	writer, err := e.getWriter(ctx)
	if err != nil {
		return "", fmt.Errorf("starting commit writer: %w", err)
	}

	oid, err := writer.Write(ctx, cfg)
	if errors.Is(err, localrepo.ErrCommitWriterUnsupported) {
		return e.repo.WriteCommit(ctx, cfg)
	}
	if err != nil {
		e.dropWriter()
	}

	return oid, err
}

// getStream returns the running merge-tree stream, starting one if there is none or if the
// context the stream was started with is done.
func (e *Engine) getStream(ctx context.Context) (mergeStream, error) {
	if e.stream != nil && isDone(e.streamDone) {
		e.dropStream()
	}

	if e.stream == nil {
		stream, err := e.backends.newMergeStream(ctx)
		if err != nil {
			return nil, err
		}
		e.stream, e.streamDone = stream, ctx.Done()
	}

	return e.stream, nil
}

// getWriter returns the running commit writer, starting one if there is none or if the context
// the writer was started with is done.
func (e *Engine) getWriter(ctx context.Context) (commitWriter, error) {
	if e.writer != nil && isDone(e.writerDone) {
		e.dropWriter()
	}

	if e.writer == nil {
		writer, err := e.backends.newCommitWriter(ctx)
		if err != nil {
			return nil, err
		}
		e.writer, e.writerDone = writer, ctx.Done()
	}

	return e.writer, nil
}

// dropStream closes the merge-tree stream so that the next merge starts a new one. The error of
// closing is dropped: the stream is broken or ended by its context, and the merge that failed
// already reports why.
func (e *Engine) dropStream() {
	_ = closeHelper(e.streamDone, e.stream.Close)
	e.stream, e.streamDone = nil, nil
}

// dropWriter closes the commit writer so that the next write starts a new one. The error of
// closing is dropped for the same reason as in dropStream.
func (e *Engine) dropWriter() {
	_ = closeHelper(e.writerDone, e.writer.Close)
	e.writer, e.writerDone = nil, nil
}

// fullCommitMessage returns the whole message of the commit. ReadCommit cuts the body of commits
// to a maximum size, so the message is read again from the raw object when it was cut.
func (e *Engine) fullCommitMessage(ctx context.Context, commit *gitalypb.GitCommit) ([]byte, error) {
	body := commit.GetBody()
	if int64(len(body)) >= commit.GetBodySize() {
		return body, nil
	}

	object, err := e.repo.ReadObject(ctx, git.ObjectID(commit.GetId()))
	if err != nil {
		return nil, fmt.Errorf("reading commit object: %w", err)
	}

	// The headers end with the first empty line. Continuation lines of multi-line headers, such
	// as signatures, start with a space and so are never empty.
	_, message, found := bytes.Cut(object, []byte("\n\n"))
	if !found || int64(len(message)) != commit.GetBodySize() {
		return nil, fmt.Errorf("commit %s: message of %d bytes does not match its size of %d bytes",
			commit.GetId(), len(message), commit.GetBodySize())
	}

	return message, nil
}

// signatureFromCommitAuthor converts a commit author to a signature, keeping its timezone. It
// mirrors the conversion of (*localrepo.Repo).Rebase.
func signatureFromCommitAuthor(author *gitalypb.CommitAuthor) *git.Signature {
	signature := &git.Signature{
		Name:  string(author.GetName()),
		Email: string(author.GetEmail()),
		When:  time.Now().In(time.UTC),
	}

	if timestamp := author.GetDate(); timestamp != nil {
		signature.When = timestamp.AsTime().In(time.UTC)
	}

	timezone := author.GetTimezone()
	if len(timezone) != 5 {
		return signature
	}

	loc, err := time.Parse("-0700", string(timezone))
	if err != nil {
		return signature
	}

	signature.When = signature.When.In(loc.Location())

	return signature
}
