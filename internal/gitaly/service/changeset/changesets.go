package changeset

import (
	"context"

	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// defaultLimit is the number of changesets ListChangesets returns when the
	// request does not set a limit.
	defaultLimit = 1000
	// maxBatchSize is the maximum number of changesets per ListChangesets
	// message.
	maxBatchSize = 1000
)

// repoPath validates repo and returns the path of its git directory, which is
// where the changeset log lives.
func (s *server) repoPath(ctx context.Context, repo *gitalypb.Repository) (string, error) {
	if err := s.deps.StorageLocator.ValidateRepository(ctx, repo); err != nil {
		return "", structerr.NewInvalidArgument("%w", err)
	}

	path, err := s.deps.StorageLocator.GetRepoPath(ctx, repo)
	if err != nil {
		return "", err
	}

	return path, nil
}

// ListChangesets streams the changesets of the repository with a sequence greater
// than after_sequence, in ascending order, up to limit changesets in total.
func (s *server) ListChangesets(req *gitalypb.ListChangesetsRequest, stream gitalypb.ChangesetService_ListChangesetsServer) error {
	ctx := stream.Context()

	source, err := s.changelogSource(ctx, req.GetRepository())
	if err != nil {
		return err
	}

	limit := uint64(req.GetLimit())
	if limit == 0 {
		limit = defaultLimit
	}

	var (
		sent  uint64
		batch []*gitalypb.Changeset
	)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		err := stream.Send(&gitalypb.ListChangesetsResponse{Changesets: batch})
		batch = nil
		return err
	}

	if err := source.Scan(ctx, req.GetAfterSequence(), func(entry changelog.Entry) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		batch = append(batch, toProto(entry))
		sent++

		if len(batch) == maxBatchSize || sent == limit {
			if err := flush(); err != nil {
				return false, err
			}
		}

		return sent < limit, nil
	}); err != nil {
		return structerr.NewInternal("reading changelog: %w", err)
	}

	return flush()
}

// toProto converts a changelog entry to its protobuf message.
func toProto(entry changelog.Entry) *gitalypb.Changeset {
	return &gitalypb.Changeset{
		Sequence:   entry.Sequence,
		Reference:  []byte(entry.Reference),
		OldOid:     entry.OldOID,
		NewOid:     entry.NewOID,
		Operation:  entry.Operation,
		Actor:      entry.Actor,
		RecordedAt: timestamppb.New(entry.RecordedAt),
		Rewritten:  entry.Rewritten,
		Members:    membersToProto(entry.Members),
	}
}

// membersToProto converts the members of a batched land to their protobuf
// messages. It returns nil when there are none.
func membersToProto(members []changelog.Member) []*gitalypb.ChangesetMember {
	if len(members) == 0 {
		return nil
	}

	out := make([]*gitalypb.ChangesetMember, 0, len(members))
	for _, m := range members {
		member := &gitalypb.ChangesetMember{
			OriginalCommitId: m.OriginalCommit,
			CommitId:         m.Commit,
			Actor:            m.Actor,
			Rewritten:        m.Rewritten,
		}
		if len(m.ResolvedPaths) > 0 {
			member.ResolvedPaths = make([][]byte, len(m.ResolvedPaths))
			for i, p := range m.ResolvedPaths {
				member.ResolvedPaths[i] = []byte(p)
			}
		}

		out = append(out, member)
	}

	return out
}
