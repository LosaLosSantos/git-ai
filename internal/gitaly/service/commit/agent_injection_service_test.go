package commit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/blob"
	"github.com/feder-cr/git-ai/internal/gitaly/service/ref"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// injectionFixture is a service with the commit, blob and ref services, and a repository whose HEAD
// is a normal branch and whose only commit that no branch reaches is a proposal under refs/graft/.
type injectionFixture struct {
	ctx          context.Context
	agent        context.Context
	cfg          config.Cfg
	repo         *gitalypb.Repository
	repoPath     string
	commits      gitalypb.CommitServiceClient
	blobs        gitalypb.BlobServiceClient
	refs         gitalypb.RefServiceClient
	main         git.ObjectID
	proposal     git.ObjectID
	proposedBlob git.ObjectID
}

func newInjectionFixture(t *testing.T, mainEntries ...gittest.TreeEntry) injectionFixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterCommitServiceServer(srv, NewServer(deps))
		gitalypb.RegisterBlobServiceServer(srv, blob.NewServer(deps))
		gitalypb.RegisterRefServiceServer(srv, ref.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	}, testserver.WithInterceptors(agentInterceptors))
	conn := dial(t, cfg.SocketPath)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	if len(mainEntries) == 0 {
		mainEntries = []gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "a\n"}}
	}
	main := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithTreeEntries(mainEntries...))
	gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
	proposal := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(main),
		gittest.WithReference("refs/graft/proposals/0123456789abcdef/1"), gittest.WithMessage("graft: proposal\n"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "proposed by a model\n"}))

	return injectionFixture{
		ctx:          ctx,
		agent:        metadata.AppendToOutgoingContext(ctx, testAgentHeader, "agent-a"),
		cfg:          cfg,
		repo:         repo,
		repoPath:     repoPath,
		commits:      gitalypb.NewCommitServiceClient(conn),
		blobs:        gitalypb.NewBlobServiceClient(conn),
		refs:         gitalypb.NewRefServiceClient(conn),
		main:         main,
		proposal:     proposal,
		proposedBlob: gittest.ResolveRevision(t, cfg, repoPath, proposal.String()+":a"),
	}
}

func drainStream(recv func() error) error {
	for {
		if err := recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// TestRevisionInjection_rpcsRefuseLineBreaks: revisions travel to git one per line on its stdin, so
// a revision with a line feed, a carriage return or a NUL is more than one revision to git, or one
// that the filter that leaves refs/graft/ out of an agent's walks reads differently from git. The
// pseudo-revision patterns after "=" took any bytes, and the request check read the whole as one
// name: "--glob=x\nrefs/graft/green/main" named a hidden reference without the check seeing it.
// Every RPC that sends revisions to git over stdin refuses them now, for every caller, with
// InvalidArgument.
func TestRevisionInjection_rpcsRefuseLineBreaks(t *testing.T) {
	t.Parallel()

	fx := newInjectionFixture(t)
	rpcs := map[string]func(context.Context, ...string) error{
		"ListCommits": func(ctx context.Context, revs ...string) error {
			stream, err := fx.commits.ListCommits(ctx, &gitalypb.ListCommitsRequest{Repository: fx.repo, Revisions: revs})
			require.NoError(t, err)
			return drainStream(func() error { _, err := stream.Recv(); return err })
		},
		"ListBlobs": func(ctx context.Context, revs ...string) error {
			stream, err := fx.blobs.ListBlobs(ctx, &gitalypb.ListBlobsRequest{Repository: fx.repo, Revisions: revs, Limit: 100})
			require.NoError(t, err)
			return drainStream(func() error { _, err := stream.Recv(); return err })
		},
		"ListLFSPointers": func(ctx context.Context, revs ...string) error {
			stream, err := fx.blobs.ListLFSPointers(ctx, &gitalypb.ListLFSPointersRequest{Repository: fx.repo, Revisions: revs, Limit: 100})
			require.NoError(t, err)
			return drainStream(func() error { _, err := stream.Recv(); return err })
		},
		"GetTagSignatures": func(ctx context.Context, revs ...string) error {
			stream, err := fx.refs.GetTagSignatures(ctx, &gitalypb.GetTagSignaturesRequest{Repository: fx.repo, TagRevisions: revs})
			require.NoError(t, err)
			return drainStream(func() error { _, err := stream.Recv(); return err })
		},
	}
	injections := map[string][]string{
		"CR after --all":            {"--glob=x\n--all\r", "main"},
		"NUL after --all":           {"--glob=x\n--all\x00", "main"},
		"name after a line feed":    {"--glob=x\nrefs/graft/proposals/0123456789abcdef/1"},
		"short name after a line":   {"--branches=x\ngraft/proposals/0123456789abcdef/1"},
		"tags pattern, then a name": {"--tags=x\nrefs/graft/green/main"},
		"CR alone in the pattern":   {"--glob=refs/*\r--all"},
		"NUL in the pattern":        {"--glob=refs/*\x00"},
		"line feed in a path":       {"main:a\nrefs/graft/proposals/0123456789abcdef/1"},
	}

	for name, call := range rpcs {
		for desc, revs := range injections {
			for who, callCtx := range map[string]context.Context{"agent": fx.agent, "other caller": fx.ctx} {
				t.Run(name+"/"+desc+"/"+who, func(t *testing.T) {
					t.Parallel()

					err := call(callCtx, revs...)
					require.Error(t, err)
					require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
				})
			}
		}
		t.Run(name+"/control, an ordinary pattern", func(t *testing.T) {
			t.Parallel()
			require.NoError(t, call(fx.agent, "--glob=refs/heads/*"))
		})
	}
}

// TestRevisionInjection_nameAndPathRPCsReachNothingHidden: the RPCs that name an object by revision
// and path (GetBlobs, TreeEntry) hand git a NUL-delimited request, where a line break is data. A
// revision with one names no object, and never the proposal's. (The plain hidden names are refused
// by the agent request check of the auth interceptor, which this server does not install: see
// TestCheckAgentRefs.)
func TestRevisionInjection_nameAndPathRPCsReachNothingHidden(t *testing.T) {
	t.Parallel()

	fx := newInjectionFixture(t)
	for desc, revision := range map[string]string{
		"line feed and a hidden name": "main\nrefs/graft/proposals/0123456789abcdef/1",
		"carriage return":             "main\rrefs/graft/proposals/0123456789abcdef/1",
		"NUL":                         "main\x00refs/graft/proposals/0123456789abcdef/1",
		"a pattern":                   "--glob=x\nrefs/graft/green/main",
	} {
		for who, callCtx := range map[string]context.Context{"agent": fx.agent, "other caller": fx.ctx} {
			t.Run("GetBlobs/"+desc+"/"+who, func(t *testing.T) {
				t.Parallel()

				stream, err := fx.blobs.GetBlobs(callCtx, &gitalypb.GetBlobsRequest{
					Repository:    fx.repo,
					RevisionPaths: []*gitalypb.GetBlobsRequest_RevisionPath{{Revision: revision, Path: []byte("a")}},
					Limit:         -1,
				})
				require.NoError(t, err)
				var data []byte
				err = drainStream(func() error {
					resp, err := stream.Recv()
					data = append(data, resp.GetData()...)
					return err
				})
				require.NotContains(t, string(data), "proposed by a model")
			})
			t.Run("TreeEntry/"+desc+"/"+who, func(t *testing.T) {
				t.Parallel()

				stream, err := fx.commits.TreeEntry(callCtx, &gitalypb.TreeEntryRequest{
					Repository: fx.repo, Revision: []byte(revision), Path: []byte("a"),
				})
				require.NoError(t, err)
				var data []byte
				_ = drainStream(func() error {
					resp, err := stream.Recv()
					data = append(data, resp.GetData()...)
					return err
				})
				require.NotContains(t, string(data), "proposed by a model")
			})
		}
	}
}

// TestRevisionInjection_otherSpellingsReachNothingHidden: spellings of a revision that are not
// control characters and that someone might try (@, @{-1}, -1) resolve against the repository's HEAD,
// a normal branch, or fail: none reaches a commit that only refs/graft/ reaches.
func TestRevisionInjection_otherSpellingsReachNothingHidden(t *testing.T) {
	t.Parallel()

	fx := newInjectionFixture(t)
	for _, revision := range []string{"@", "@{-1}", "-1", "HEAD", "HEAD@{1}", "@{upstream}", "main@{0}", "FETCH_HEAD", "ORIG_HEAD"} {
		t.Run(revision, func(t *testing.T) {
			t.Parallel()

			for who, callCtx := range map[string]context.Context{"agent": fx.agent, "other caller": fx.ctx} {
				stream, err := fx.commits.ListCommits(callCtx, &gitalypb.ListCommitsRequest{Repository: fx.repo, Revisions: []string{revision}})
				require.NoError(t, err)
				var ids []string
				_ = drainStream(func() error {
					resp, err := stream.Recv()
					for _, c := range resp.GetCommits() {
						ids = append(ids, c.GetId())
					}
					return err
				})
				require.NotContains(t, ids, fx.proposal.String(), "%s: %s", who, revision)
			}
		})
	}
}

// TestRevisionPaths_stillAcceptWhitespaceAndNonASCII: the refusal of control characters leaves the
// paths that people have: a tab, a space, a non-ASCII name, in ListBlobs and GetBlobs.
func TestRevisionPaths_stillAcceptWhitespaceAndNonASCII(t *testing.T) {
	t.Parallel()

	paths := map[string]string{
		"a b.txt":      "space\n",
		"tab\there":    "tab\n",
		"é à ü.txt":    "latin\n",
		"日本語.txt":      "cjk\n",
		"emoji 🙂.txt":  "emoji\n",
		"quote'\"s.go": "quote\n",
	}
	var entries []gittest.TreeEntry
	for path, content := range paths {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: path, Content: content})
	}
	fx := newInjectionFixture(t, entries...)

	for path, content := range paths {
		for who, callCtx := range map[string]context.Context{"agent": fx.agent, "other caller": fx.ctx} {
			t.Run("ListBlobs/"+path+"/"+who, func(t *testing.T) {
				t.Parallel()

				want := gittest.ResolveRevision(t, fx.cfg, fx.repoPath, "main:"+path)
				stream, err := fx.blobs.ListBlobs(callCtx, &gitalypb.ListBlobsRequest{
					Repository: fx.repo, Revisions: []string{"main:" + path}, Limit: 10,
				})
				require.NoError(t, err)
				var oids []string
				require.NoError(t, drainStream(func() error {
					resp, err := stream.Recv()
					for _, b := range resp.GetBlobs() {
						oids = append(oids, b.GetOid())
					}
					return err
				}))
				require.Equal(t, []string{want.String()}, oids)
			})
			t.Run("GetBlobs/"+path+"/"+who, func(t *testing.T) {
				t.Parallel()

				stream, err := fx.blobs.GetBlobs(callCtx, &gitalypb.GetBlobsRequest{
					Repository:    fx.repo,
					RevisionPaths: []*gitalypb.GetBlobsRequest_RevisionPath{{Revision: "main", Path: []byte(path)}},
					Limit:         -1,
				})
				require.NoError(t, err)
				var data bytes.Buffer
				require.NoError(t, drainStream(func() error {
					resp, err := stream.Recv()
					data.Write(resp.GetData())
					return err
				}))
				require.Equal(t, content, data.String())
			})
		}
	}
}
