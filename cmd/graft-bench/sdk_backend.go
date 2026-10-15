package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitalyauth "github.com/feder-cr/open_scion/auth"
	"github.com/feder-cr/open_scion/client"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

// zeroOID is the object ID that means "no object", used to require that a
// branch does not exist yet.
const zeroOID = "0000000000000000000000000000000000000000"

// createRepository creates the repository relPath on storage through the
// RepositoryService of the server, so that the server creates it inside its
// own storage. It fails if the repository exists. token must be the shared
// token of the server.
func createRepository(ctx context.Context, address, token, storage, relPath string) error {
	var grpcOpts []grpc.DialOption
	if token != "" {
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)))
	}
	conn, err := client.DialContext(ctx, address, client.WithGrpcOptions(grpcOpts))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = gitalypb.NewRepositoryServiceClient(conn).CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
		Repository: &gitalypb.Repository{StorageName: storage, RelativePath: relPath},
	})
	return err
}

// SDKBackend runs the benchmark against a Graft server through the public
// Go SDK.
type SDKBackend struct {
	repo *sdk.Repo
}

// NewSDKBackend returns a backend for the repository relPath on storage of
// the server at address. The client is not closed by the backend; the caller
// owns it.
func NewSDKBackend(client *sdk.Client, relPath string) *SDKBackend {
	return &SDKBackend{repo: client.Repo(relPath)}
}

// benchUser is the user the benchmark writes as. Agents are distinguished by
// their gl_id, so each agent shows up as its own actor in the changelog.
func benchUser(agent int) sdk.User {
	name := fmt.Sprintf("agent-%03d", agent)
	return sdk.User{
		ID:       name,
		Name:     name,
		Email:    name + "@graft-bench.invalid",
		Username: name,
	}
}

// Prepare returns the tip of the branch. If the branch does not exist it is
// created with an empty shared file. Any other error is returned as is, so a
// transient failure is never mistaken for a missing branch.
//
// A branch that an earlier run landed on is refused (errBranchUsed): it holds
// the files of the changes already, so every change of the new run would fail
// to commit. Failing here says why, instead of reporting every change failed.
func (b *SDKBackend) Prepare(ctx context.Context, cfg Config) (string, error) {
	tip, err := b.repo.ResolveRevision(ctx, cfg.Branch)
	if err == nil {
		if err := b.checkUnused(ctx, cfg.Branch, tip); err != nil {
			return "", err
		}
		return tip, nil
	}
	if !sdk.IsNotFound(err) {
		return "", err
	}

	res, err := b.repo.CommitFiles(ctx, sdk.CommitOptions{
		Branch:         cfg.Branch,
		ExpectedOldOID: zeroOID,
		Message:        "graft-bench: initial commit",
		Author:         sdk.Signature{Name: "graft-bench", Email: "bench@graft-bench.invalid"},
		User:           sdk.User{ID: "graft-bench", Name: "graft-bench", Email: "bench@graft-bench.invalid", Username: "graft-bench"},
		Actions: []sdk.Action{
			sdk.Put{Path: sharedFilePath, Content: []byte(sharedFileHeader)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	return res.CommitID, nil
}

// errBranchUsed is returned by Prepare for a branch an earlier run used.
var errBranchUsed = errors.New("every run needs a fresh --branch")

// checkUnused fails with errBranchUsed if the tip of branch holds the
// directory of the changes (agentsDir), which only a run of the benchmark
// writes.
func (b *SDKBackend) checkUnused(ctx context.Context, branch, tip string) error {
	_, err := b.repo.ReadFile(ctx, tip, agentsDir)
	switch {
	case sdk.IsNotFound(err):
		return nil
	case err == nil || status.Code(err) == codes.FailedPrecondition:
		// FailedPrecondition: the path is a directory, the usual case.
		return fmt.Errorf("branch %q already holds %s/ from an earlier run: %w", branch, agentsDir, errBranchUsed)
	default:
		return fmt.Errorf("check branch: %w", err)
	}
}

// Commit writes the change on a private branch based on base. The private
// branch name is unique per change and target branch, so concurrent agents
// never collide and runs on different branches can share a repository. A
// target branch is used by one run only (see Prepare), since the files of its
// changes exist after the run.
func (b *SDKBackend) Commit(ctx context.Context, c Change, base string) (string, error) {
	actions := []sdk.Action{sdk.Put{Path: c.Path(), Content: c.Content()}}
	if c.Shared {
		actions = append(actions, sdk.Put{Path: sharedFilePath, Content: c.SharedContent(), Overwrite: true})
	}

	message := fmt.Sprintf("graft-bench: agent %d change %d", c.Agent, c.Seq)
	user := benchUser(c.Agent)
	res, err := b.repo.CommitFiles(ctx, sdk.CommitOptions{
		Branch:    fmt.Sprintf("graft-bench/%s/agent-%03d/change-%04d", c.Target, c.Agent, c.Seq),
		StartRev:  base,
		Message:   message,
		Author:    sdk.Signature{Name: user.Name, Email: user.Email},
		User:      user,
		Timestamp: time.Now(),
		Actions:   actions,
	})
	if err != nil {
		return "", err
	}

	return res.CommitID, nil
}

// Land lands commit on the target branch of change c with a rebase. In
// land-resolve mode the server is asked to resolve merge conflicts.
func (b *SDKBackend) Land(ctx context.Context, cfg Config, c Change, commit string) (LandOutcome, error) {
	res, err := b.repo.Land(ctx, landOptions(cfg, c, commit))
	if err != nil {
		return failedLandOutcome(err), err
	}

	return landOutcome(res), nil
}

// keptMovingAttempts matches the number of attempts in the message of a land
// that gave up because the branch kept moving: "branch "x" kept moving, giving
// up after 20 attempts: ...".
var keptMovingAttempts = regexp.MustCompile(`giving up after (\d+) attempts`)

// failedLandOutcome converts a failed SDK land into the benchmark's outcome.
// A land that gave up because the branch kept moving made every attempt it
// was allowed. The server reports that count only in the error message (its
// ReferenceUpdateError detail has no field for it), so it is read from there:
// those attempts are server work that the report must not hide. Other
// failures report no attempts, since the server does not say how many it made.
func failedLandOutcome(err error) LandOutcome {
	out := LandOutcome{Conflict: sdk.IsMergeConflict(err)}
	if !sdk.IsReferenceUpdateConflict(err) {
		return out
	}

	if m := keptMovingAttempts.FindStringSubmatch(status.Convert(err).Message()); m != nil {
		if n, perr := strconv.ParseInt(m[1], 10, 32); perr == nil {
			out.Attempts = int32(n)
		}
	}

	return out
}

// landOptions returns the SDK options that land commit for change c under
// cfg. Landing is always a rebase, which is the strategy whose conflicts the
// benchmark measures.
func landOptions(cfg Config, c Change, commit string) sdk.LandOptions {
	return sdk.LandOptions{
		Branch:           c.Target,
		Commit:           commit,
		User:             benchUser(c.Agent),
		Strategy:         sdk.LandRebase,
		MaxAttempts:      cfg.MaxAttempts,
		ResolveConflicts: cfg.Mode == ModeLandResolve,
	}
}

// landOutcome converts a successful SDK land result, including the batch the
// server reported for it, into the benchmark's outcome.
func landOutcome(res *sdk.LandResult) LandOutcome {
	return LandOutcome{
		Attempts:      res.Attempts,
		Rewritten:     res.Rewritten,
		AlreadyLanded: res.AlreadyLanded,
		Resolved:      len(res.ResolvedPaths),
		BatchSize:     int(res.Batch.Size),
		BatchPosition: int(res.Batch.Position),
	}
}
