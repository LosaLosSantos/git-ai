package sdk

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// commitContentChunkSize bounds the payload of one content message sent to
// UserCommitFiles.
const commitContentChunkSize = 64 * 1024

// Signature identifies the author of a commit.
type Signature struct {
	Name  string
	Email string
}

// User identifies the acting user of a write. It is used for authorization
// by hooks and as committer of rewritten commits. ID is the GitLab user ID
// (gl_id); when empty, the server fills it in from the authenticated
// identity.
type User struct {
	ID       string
	Name     string
	Email    string
	Username string
}

func (u User) toProto() *gitalypb.User {
	return &gitalypb.User{
		GlId:       u.ID,
		Name:       []byte(u.Name),
		Email:      []byte(u.Email),
		GlUsername: u.Username,
	}
}

// CommitOptions describes a commit built on the server by CommitFiles.
type CommitOptions struct {
	// Branch is the branch to point at the new commit, without the
	// refs/heads/ prefix.
	Branch string
	// StartRev is the revision the new commit is built on. If empty, the
	// current tip of Branch is used.
	StartRev string
	// ExpectedOldOID, if set, makes the update fail unless Branch points at
	// this object ID. Use the zero object ID to require that Branch does not
	// exist yet.
	ExpectedOldOID string
	// Message is the commit message.
	Message string
	// Author is the commit author.
	Author Signature
	// User is the user the write is performed as.
	User User
	// Timestamp is used as author and committer date. If zero, the server's
	// current time is used.
	Timestamp time.Time
	// Actions are applied in order to the tree of the start revision.
	Actions []Action
}

// CommitResult describes the branch update made by CommitFiles.
type CommitResult struct {
	// CommitID is the ID of the created commit.
	CommitID string
	// BranchCreated is true if the branch did not exist before.
	BranchCreated bool
}

// Action is a single tree change applied by CommitFiles. The implementations
// are Put, Delete, Move and Chmod.
type Action interface {
	isAction()
}

// Put creates the file at Path with Content. If Overwrite is true, an
// existing file is replaced instead, and Executable is ignored.
type Put struct {
	Path       string
	Content    []byte
	Executable bool
	Overwrite  bool
}

// Delete removes the file at Path.
type Delete struct {
	Path string
}

// Move renames the file From to To without changing its content.
type Move struct {
	From string
	To   string
}

// Chmod sets the executable bit of the file at Path.
type Chmod struct {
	Path       string
	Executable bool
}

func (Put) isAction()    {}
func (Delete) isAction() {}
func (Move) isAction()   {}
func (Chmod) isAction()  {}

// CommitFiles builds a commit on the server by applying opts.Actions to the
// tree of the start revision, and points opts.Branch at it. Content is
// streamed to the server in chunks.
//
// Errors caused by the actions are returned with the server's gRPC status. For
// example, creating a file that already exists fails with AlreadyExists, and
// a stale ExpectedOldOID fails with FailedPrecondition.
func (r *Repo) CommitFiles(ctx context.Context, opts CommitOptions) (*CommitResult, error) {
	if err := validateCommitOptions(opts); err != nil {
		return nil, err
	}

	header := &gitalypb.UserCommitFilesRequestHeader{
		Repository:        r.repository(),
		User:              opts.User.toProto(),
		BranchName:        []byte(trimBranchPrefix(opts.Branch)),
		CommitMessage:     []byte(opts.Message),
		CommitAuthorName:  []byte(opts.Author.Name),
		CommitAuthorEmail: []byte(opts.Author.Email),
		ExpectedOldOid:    opts.ExpectedOldOID,
	}
	if !opts.Timestamp.IsZero() {
		header.Timestamp = timestamppb.New(opts.Timestamp)
	}
	if opts.StartRev != "" {
		startSHA, err := r.ResolveRevision(ctx, opts.StartRev)
		if err != nil {
			return nil, err
		}
		header.StartSha = startSHA
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := r.client.operations.UserCommitFiles(ctx)
	if err != nil {
		return nil, err
	}

	if err := sendCommitFiles(stream, header, opts.Actions); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	// On a server-side failure Send returns io.EOF; CloseAndRecv then carries
	// the actual error.
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	if resp.GetIndexError() != "" {
		return nil, status.Error(codes.FailedPrecondition, resp.GetIndexError())
	}
	if resp.GetPreReceiveError() != "" {
		return nil, status.Error(codes.FailedPrecondition, resp.GetPreReceiveError())
	}

	return &CommitResult{
		CommitID:      resp.GetBranchUpdate().GetCommitId(),
		BranchCreated: resp.GetBranchUpdate().GetBranchCreated(),
	}, nil
}

func validateCommitOptions(opts CommitOptions) error {
	switch {
	case trimBranchPrefix(opts.Branch) == "":
		return status.Error(codes.InvalidArgument, "branch is required")
	case opts.Message == "":
		return status.Error(codes.InvalidArgument, "commit message is required")
	case len(opts.Actions) == 0:
		return status.Error(codes.InvalidArgument, "at least one action is required")
	}

	for _, action := range opts.Actions {
		if err := validateAction(action); err != nil {
			return err
		}
	}

	return nil
}

func validateAction(action Action) error {
	switch a := action.(type) {
	case Put:
		if a.Path == "" {
			return status.Error(codes.InvalidArgument, "put: path is required")
		}
	case *Put:
		return validateAction(*a)
	case Delete:
		if a.Path == "" {
			return status.Error(codes.InvalidArgument, "delete: path is required")
		}
	case *Delete:
		return validateAction(*a)
	case Move:
		if a.From == "" || a.To == "" {
			return status.Error(codes.InvalidArgument, "move: from and to are required")
		}
	case *Move:
		return validateAction(*a)
	case Chmod:
		if a.Path == "" {
			return status.Error(codes.InvalidArgument, "chmod: path is required")
		}
	case *Chmod:
		return validateAction(*a)
	case nil:
		return status.Error(codes.InvalidArgument, "action must not be nil")
	default:
		return status.Errorf(codes.InvalidArgument, "unsupported action type %T", action)
	}

	return nil
}

// sendCommitFiles streams the header and all actions to UserCommitFiles.
func sendCommitFiles(
	stream gitalypb.OperationService_UserCommitFilesClient,
	header *gitalypb.UserCommitFilesRequestHeader,
	actions []Action,
) error {
	if err := stream.Send(&gitalypb.UserCommitFilesRequest{
		UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{Header: header},
	}); err != nil {
		return err
	}

	for _, action := range actions {
		if err := sendAction(stream, action); err != nil {
			return err
		}
	}

	return nil
}

func sendAction(stream gitalypb.OperationService_UserCommitFilesClient, action Action) error {
	send := func(actionHeader *gitalypb.UserCommitFilesActionHeader) error {
		return stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
				Action: &gitalypb.UserCommitFilesAction{
					UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Header{Header: actionHeader},
				},
			},
		})
	}

	switch a := action.(type) {
	case Put:
		return sendPut(stream, a, send)
	case *Put:
		return sendPut(stream, *a, send)
	case Delete:
		return send(&gitalypb.UserCommitFilesActionHeader{
			Action:   gitalypb.UserCommitFilesActionHeader_DELETE,
			FilePath: []byte(a.Path),
		})
	case *Delete:
		return send(&gitalypb.UserCommitFilesActionHeader{
			Action:   gitalypb.UserCommitFilesActionHeader_DELETE,
			FilePath: []byte(a.Path),
		})
	case Move:
		return send(&gitalypb.UserCommitFilesActionHeader{
			Action:       gitalypb.UserCommitFilesActionHeader_MOVE,
			FilePath:     []byte(a.To),
			PreviousPath: []byte(a.From),
			InferContent: true,
		})
	case *Move:
		return send(&gitalypb.UserCommitFilesActionHeader{
			Action:       gitalypb.UserCommitFilesActionHeader_MOVE,
			FilePath:     []byte(a.To),
			PreviousPath: []byte(a.From),
			InferContent: true,
		})
	case Chmod:
		return send(&gitalypb.UserCommitFilesActionHeader{
			Action:          gitalypb.UserCommitFilesActionHeader_CHMOD,
			FilePath:        []byte(a.Path),
			ExecuteFilemode: a.Executable,
		})
	case *Chmod:
		return sendAction(stream, *a)
	default:
		return status.Errorf(codes.InvalidArgument, "unsupported action type %T", action)
	}
}

func sendPut(
	stream gitalypb.OperationService_UserCommitFilesClient,
	put Put,
	send func(*gitalypb.UserCommitFilesActionHeader) error,
) error {
	header := &gitalypb.UserCommitFilesActionHeader{
		Action:   gitalypb.UserCommitFilesActionHeader_CREATE,
		FilePath: []byte(put.Path),
	}
	if put.Overwrite {
		header.Action = gitalypb.UserCommitFilesActionHeader_UPDATE
	} else {
		header.ExecuteFilemode = put.Executable
	}

	if err := send(header); err != nil {
		return err
	}

	for content := put.Content; len(content) > 0; {
		n := min(commitContentChunkSize, len(content))
		if err := stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
				Action: &gitalypb.UserCommitFilesAction{
					UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{Content: content[:n]},
				},
			},
		}); err != nil {
			return err
		}
		content = content[n:]
	}

	return nil
}

// trimBranchPrefix turns "refs/heads/main" into "main".
func trimBranchPrefix(branch string) string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(branch, prefix) {
		return branch[len(prefix):]
	}
	return branch
}
