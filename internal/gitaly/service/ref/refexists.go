package ref

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// danglingReferenceError is returned when a reference exists but its target object is missing.
type danglingReferenceError struct {
	reference string
	oid       string
}

func (e danglingReferenceError) Error() string {
	return fmt.Sprintf("reference %q points to missing object %s", e.reference, e.oid)
}

// RefExists returns true if the given reference exists. The ref must start with the string `ref/`
func (s *server) RefExists(ctx context.Context, in *gitalypb.RefExistsRequest) (*gitalypb.RefExistsResponse, error) {
	if err := s.locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	ref := string(in.GetRef())

	if !isValidRefName(ref) {
		return nil, structerr.NewInvalidArgument("invalid refname")
	}

	repo := s.localRepoFactory.Build(in.GetRepository())

	exists, err := s.refExists(ctx, repo, ref)
	if err != nil {
		var danglingErr danglingReferenceError
		if errors.As(err, &danglingErr) {
			return nil, structerr.NewFailedPrecondition("%w", err).
				WithDetail(&gitalypb.RefExistsError{
					Error: &gitalypb.RefExistsError_UnresolvableReference{
						UnresolvableReference: &gitalypb.ReferenceUnresolvableError{
							ReferenceName: []byte(danglingErr.reference),
							Oid:           danglingErr.oid,
						},
					},
				}).
				WithMetadata("reference", ref)
		}

		return nil, structerr.NewInternal("%w", err)
	}

	return &gitalypb.RefExistsResponse{Value: exists}, nil
}

func (s *server) refExists(ctx context.Context, repo *localrepo.Repo, ref string) (bool, error) {
	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:  "show-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--verify"}, gitcmd.Flag{Name: "--quiet"}},
		Args:  []string{ref},
	})
	if err != nil {
		return false, err
	}

	err = cmd.Wait()
	if err == nil {
		return true, nil
	}

	code, ok := command.ExitStatus(err)
	if !ok {
		return false, err
	}

	switch code {
	case 1:
		return false, nil
	case 128:
		// show-ref --verify exits 128 for fatal errors; one known cause is a reference
		// whose target object is missing. danglingReference confirms that specific case.
		if danglingErr, ok := danglingReference(ctx, repo, ref); ok {
			return false, danglingErr
		}
	}

	return false, err
}

// danglingReference reports whether the reference resolves to an object ID whose object is missing.
// Anything that doesn't conclusively confirm this returns false so the caller surfaces the original
// error as INTERNAL instead of misclassifying it.
func danglingReference(ctx context.Context, repo *localrepo.Repo, ref string) (danglingReferenceError, bool) {
	oid, err := repo.ResolveRevision(ctx, git.Revision(ref))
	if err != nil {
		return danglingReferenceError{}, false
	}

	// The reference resolves to an object ID; read the object to confirm it exists.
	// A missing object means the reference is dangling.
	var invalidObjectErr localrepo.InvalidObjectError
	if _, err := repo.ReadObjectInfo(ctx, oid.Revision()); errors.As(err, &invalidObjectErr) {
		return danglingReferenceError{reference: ref, oid: oid.String()}, true
	}

	return danglingReferenceError{}, false
}

func isValidRefName(refName string) bool {
	return strings.HasPrefix(refName, "refs/")
}
