// Package rebase decides how a change lands onto a branch and replays its commits. Plan makes the
// decision from one listing of the range the change adds, made exact when the change diverges (see
// revrange), and an Engine replays the commits of a rebase through long-lived git helper processes. Together
// they produce the same commits as (*localrepo.Repo).Rebase with reapplied cherry-picks, without
// starting several git processes per commit.
package rebase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
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

// Engine replays commits within one batch attempt using one merge-tree stream and one commit
// writer, started lazily and closed by Close. An Engine is not safe for concurrent use.
//
// The helper processes are started with the context of the call that needs them and die with
// it. A helper whose context is done is replaced by a new one on the next use, so a canceled
// context of one Replay does not fail the later Replay calls of the Engine. A helper that
// fails with an error other than a merge conflict is replaced in the same way, so that one
// failed replay does not fail the replays that follow it.
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

// Option configures an Engine.
type Option func(*engineConfig)

type engineConfig struct {
	helperOpts []localrepo.HelperOption
}

// WithDeferredDurability starts the helper processes without fsync of the loose objects they write.
// The caller takes over their durability: a quarantine's contract is that every object file in it
// is durable before the quarantine migration moves it into the repository, so the caller must make
// the objects durable before the migration, not rely on it. The batch attempts of UserLand do: they
// pack the objects of their quarantine into one fsynced pack before the update (doc/graft/PLAN8.md,
// section 3.2).
func WithDeferredDurability() Option {
	return func(cfg *engineConfig) {
		cfg.helperOpts = append(cfg.helperOpts, localrepo.WithoutObjectFsync())
	}
}

// New returns an Engine for repo.
func New(repo *localrepo.Repo, opts ...Option) *Engine {
	var cfg engineConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	return newEngine(repo, backends{
		newMergeStream: func(ctx context.Context) (mergeStream, error) {
			stream, err := repo.NewMergeTreeStream(ctx, cfg.helperOpts...)
			if err != nil {
				return nil, err
			}
			return stream, nil
		},
		newCommitWriter: func(ctx context.Context) (commitWriter, error) {
			writer, err := repo.NewCommitWriter(ctx, cfg.helperOpts...)
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

// Replay replays the commits of todo, oldest first, onto upstream and returns the last commit
// written, or upstream when every commit became empty and was dropped. It is the replay of
// (*localrepo.Repo).Rebase: a commit whose merge onto the running tip changes nothing is dropped
// unless it is itself empty, each commit keeps its author and message, and its committer is
// committer, or its own committer when committer is nil. A commit that does not apply cleanly
// returns *localrepo.RebaseConflictError. The todo list comes from LandPlan.Replay.
func (e *Engine) Replay(ctx context.Context, upstream git.ObjectID, todo []git.ObjectID, committer *git.Signature) (git.ObjectID, error) {
	objectHash, err := e.repo.ObjectHash(ctx)
	if err != nil {
		return "", structerr.NewInternal("getting object hash %w", err)
	}

	upstreamCommit, err := e.repo.ReadCommit(ctx, git.Revision(upstream))
	if err != nil {
		return "", fmt.Errorf("reading upstream commit: %w", err)
	}

	oursCommitOID := upstream
	oursTreeOID := git.ObjectID(upstreamCommit.GetTreeId())
	for _, todoItem := range todo {
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
				return "", &localrepo.RebaseConflictError{
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
		commitCommitter := committer
		if commitCommitter == nil {
			commitCommitter = signatureFromCommitAuthor(theirsCommit.GetCommitter())
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
			CommitterName:  commitCommitter.Name,
			CommitterEmail: commitCommitter.Email,
			CommitterDate:  commitCommitter.When,
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
