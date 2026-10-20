package resolve

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// checkResolverName names graft-resolve in a check response, for the server's Graft-Checked-By
// trailer.
const checkResolverName = "graft-resolve"

// Check answers an OperationCheck request: it checks an agent's resolution of the request's files
// with the checks that an llm answer gets, so that one validation path applies to AI answers and
// agent resolutions (PLAN10 12). For every written file: the answer rules (action, mode, size,
// conflict markers), the syntax check (syntax.CheckMerge with the request's sides), then the
// configured checks for the path. A deletion only needs a valid action.
//
// A file that fails is a verdict, not an error. An error means the request could not be checked
// (malformed, or the context ended); the server fails closed on it.
func (r *Resolver) Check(ctx context.Context, req protocol.Request) (protocol.CheckResponse, error) {
	if req.Operation != protocol.OperationCheck {
		return protocol.CheckResponse{}, fmt.Errorf("%w: operation %q is not %q", ErrInvalidRequest, req.Operation, protocol.OperationCheck)
	}
	if err := validateRequest(req); err != nil {
		return protocol.CheckResponse{}, err
	}
	files := make(map[string]protocol.FileConflict, len(req.Files))
	for _, f := range req.Files {
		files[f.Path] = f
	}
	if len(req.Resolution) != len(req.Files) {
		return protocol.CheckResponse{}, fmt.Errorf("%w: the resolution has %d files, the request %d", ErrInvalidRequest, len(req.Resolution), len(req.Files))
	}
	seen := make(map[string]bool, len(req.Resolution))
	for _, res := range req.Resolution {
		if _, ok := files[res.Path]; !ok || seen[res.Path] {
			return protocol.CheckResponse{}, fmt.Errorf("%w: the resolution names %q, which is not a conflicting file or is named twice", ErrInvalidRequest, res.Path)
		}
		seen[res.Path] = true
	}
	maxBytes := req.Limits.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = protocol.DefaultMaxFileBytes
	}

	out := protocol.CheckResponse{Version: protocol.Version, Operation: protocol.OperationCheck, Resolver: checkResolverName}
	for _, res := range req.Resolution {
		verdict, err := r.checkFile(ctx, files[res.Path], Resolution{Action: res.Action, Mode: res.Mode, Content: res.Content}, maxBytes)
		if err != nil {
			return protocol.CheckResponse{}, err
		}
		out.Files = append(out.Files, verdict)
	}
	return out, nil
}

func (r *Resolver) checkFile(ctx context.Context, f protocol.FileConflict, res Resolution, maxBytes int64) (protocol.CheckFile, error) {
	verdict := protocol.CheckFile{Path: f.Path, Syntax: protocol.SyntaxNone}
	if err := checkResolution(res, maxBytes); err != nil {
		verdict.Reason = err.Error()
		return verdict, nil
	}
	if res.Action == protocol.ActionDelete {
		verdict.OK = true
		return verdict, nil
	}

	if res.Mode != protocol.ModeSymlink {
		if _, ok := syntax.LanguageOf(f.Path); ok {
			checked, err := syntax.CheckMerge(ctx, f.Path, res.Content, sideContent(f.Ours), sideContent(f.Theirs))
			var se *syntax.Error
			switch {
			case errors.As(err, &se):
				verdict.Syntax = protocol.SyntaxChecked
				verdict.Reason = se.Error()
				return verdict, nil
			case err != nil:
				return protocol.CheckFile{}, fmt.Errorf("checking the syntax of %q: %w", f.Path, err)
			case checked:
				verdict.Syntax = protocol.SyntaxChecked
			default:
				verdict.Syntax = protocol.SyntaxUnchecked
			}
		}
	}

	if err := r.runChecks(ctx, f.Path, res); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return protocol.CheckFile{}, ctxErr
		}
		verdict.Reason = "check failed: " + err.Error()
		return verdict, nil
	}
	verdict.OK = true
	return verdict, nil
}
