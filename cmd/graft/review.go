package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// reviewUsage lists the subcommands of graft review.
const reviewUsage = "review requires a subcommand: create, show, list, comment, approve, request-changes, update or close"

// reviewFlags holds the flags of graft review. Each subcommand reads only some
// of them, as its help text says.
type reviewFlags struct {
	base        string
	title       string
	description string
	head        string
	body        string
	path        string
	line        int
	repo        string
	all         bool
	landed      bool
}

// reviewSession is the connection of one review command: the review handle of
// the repository, and the workspace root when the command runs in a workspace.
type reviewSession struct {
	repo    *sdk.Repo
	backend workspace.Backend
	root    string
}

// reviewCommand returns the review command. Its flags live in one reviewFlags
// value per command line, so the bind and run functions see the same flags.
func reviewCommand() command {
	f := &reviewFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.StringVar(&o.branch, "branch", "", "create, list: target branch")
			fs.StringVar(&f.base, "base", "", "create: base commit (default: the merge base of head and the target branch)")
			fs.StringVar(&f.title, "title", "", "create: one-line title")
			fs.StringVar(&f.description, "description", "", "create: free text")
			fs.StringVar(&f.head, "head", "", "create, update: head commit ID (default: the pending commit of the workspace); "+
				"approve, request-changes: the head that was reviewed, in full or as a prefix (default: the current head of the review)")
			fs.StringVar(&f.repo, "repo", "", "repository path, used without a workspace together with -address and -storage")
			fs.StringVar(&f.body, "body", "", "comment, approve, request-changes: message")
			fs.StringVar(&f.path, "path", "", "comment: file the comment is anchored to")
			fs.IntVar(&f.line, "line", 0, "comment: 1-based line of --path (at most 2147483647)")
			fs.BoolVar(&f.all, "all", false, "list: include landed and closed reviews")
			fs.BoolVar(&f.landed, "landed", false, "close: mark the review landed instead of closed")
			fs.BoolVar(&o.jsonOut, "json", false, "show, list: print reviews as JSON, one object per review "+
				"(bytes fields such as title, description, paths and bodies are base64, as protojson encodes them)")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdReview(ctx, out, o, f, args)
		},
	}
}

// cmdReview dispatches a review subcommand. Arguments are checked before the
// server is contacted.
func cmdReview(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", reviewUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return reviewCreate(ctx, out, o, f, rest)
	case "show":
		return reviewShow(ctx, out, o, f, rest)
	case "list":
		return reviewList(ctx, out, o, f, rest)
	case "comment":
		return reviewComment(ctx, out, o, f, rest)
	case "approve":
		return reviewVerdict(ctx, out, o, f, rest, false)
	case "request-changes":
		return reviewVerdict(ctx, out, o, f, rest, true)
	case "update":
		return reviewUpdate(ctx, out, o, f, rest)
	case "close":
		return reviewClose(ctx, out, o, f, rest)
	}
	return usagef("unknown review subcommand %q: %s", sub, reviewUsage)
}

func reviewCreate(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	if len(args) != 0 {
		return usagef("review create takes flags only: create --branch B --title T [--base C] [--head H] [--description D]")
	}
	if o.branch == "" {
		return usagef("review create requires --branch B")
	}
	if strings.TrimSpace(f.title) == "" {
		return usagef("review create requires --title T")
	}

	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	head, err := s.headOr(f.head)
	if err != nil {
		return err
	}
	review, err := s.repo.CreateReview(ctx, sdk.CreateReviewOptions{
		TargetBranch: o.branch,
		Head:         head,
		Base:         f.base,
		Title:        f.title,
		Description:  f.description,
	})
	if err != nil {
		return mapError(err)
	}
	fmt.Fprintf(out, "opened review %s: %s (%s <- %s)\n", displayText(review.GetId()), displayText(string(review.GetTitle())), displayText(o.branch), shortOID(head))
	return nil
}

func reviewShow(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	id, err := reviewID("show", args)
	if err != nil {
		return err
	}
	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	review, err := s.repo.GetReview(ctx, id)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeReviewJSON(out, review)
	}
	formatReview(out, review)
	return nil
}

func reviewList(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	if len(args) != 0 {
		return usagef("review list takes flags only: list [--branch B] [--all] [--json]")
	}
	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	reviews, err := s.repo.ListReviews(ctx, sdk.ListReviewsOptions{TargetBranch: o.branch, IncludeClosed: f.all})
	if err != nil {
		return mapError(err)
	}
	for _, r := range reviews {
		if o.jsonOut {
			if err := writeReviewJSON(out, r); err != nil {
				return err
			}
			continue
		}
		// The title and the branch come from other clients. displayText keeps a
		// tab or a newline in them from forging a row of the listing.
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", displayText(r.GetId()), reviewStateName(r.GetState()),
			displayText(string(r.GetTargetBranch())), shortOID(r.GetHeadCommitId()), displayText(string(r.GetTitle())))
	}
	return nil
}

func reviewComment(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	id, err := reviewID("comment", args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(f.body) == "" {
		return usagef("review comment requires --body B")
	}
	if f.line != 0 && f.path == "" {
		return usagef("review comment --line requires --path P")
	}
	if f.line < 0 || f.line > math.MaxInt32 {
		return usagef("review comment --line must be between 0 and %d", math.MaxInt32)
	}
	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	comment, err := s.repo.CommentReview(ctx, sdk.CommentReviewOptions{ID: id, Body: f.body, Path: f.path, Line: int32(f.line)})
	if err != nil {
		return mapError(err)
	}
	if f.path != "" {
		fmt.Fprintf(out, "commented on review %s at %s:%d\n", id, displayText(string(comment.GetPath())), comment.GetLine())
		return nil
	}
	fmt.Fprintf(out, "commented on review %s\n", id)
	return nil
}

// reviewVerdict records an approval, or a change request when changes is set.
// The verdict is given on --head, or on the current head of the review when
// --head is not set. The server refuses it if the review has moved to another
// head since the reviewed one.
func reviewVerdict(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string, changes bool) error {
	name := "approve"
	if changes {
		name = "request-changes"
	}
	id, err := reviewID(name, args)
	if err != nil {
		return err
	}
	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	opts := sdk.VerdictOptions{ID: id, Body: f.body}
	if f.head != "" {
		head, err := s.verdictHead(ctx, id, f.head)
		if err != nil {
			return mapError(err)
		}
		opts.Head = head
	}
	if changes {
		review, err := s.repo.RequestChangesAt(ctx, opts)
		if err != nil {
			return mapError(err)
		}
		fmt.Fprintf(out, "requested changes on review %s at %s (approved: %s)\n", id, shortOID(review.GetHeadCommitId()), approvedNote(review.GetApproved()))
		return nil
	}
	review, err := s.repo.ApproveReviewAt(ctx, opts)
	if err != nil {
		return mapError(err)
	}
	fmt.Fprintf(out, "approved review %s at %s (approved: %s)\n", id, shortOID(review.GetHeadCommitId()), approvedNote(review.GetApproved()))
	return nil
}

// verdictHead returns the full commit ID that head names when it is a prefix of
// the current head of the review. The server compares full IDs only, and show
// prints short ones, so a prefix copied from show is resolved here. Any other
// head is returned unchanged, and the server refuses it unless it is the current
// head.
func (s *reviewSession) verdictHead(ctx context.Context, id, head string) (string, error) {
	review, err := s.repo.GetReview(ctx, id)
	if err != nil {
		return "", err
	}
	current := review.GetHeadCommitId()
	if current != head && strings.HasPrefix(current, head) {
		return current, nil
	}
	return head, nil
}

func reviewUpdate(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	id, err := reviewID("update", args)
	if err != nil {
		return err
	}
	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	head, err := s.headOr(f.head)
	if err != nil {
		return err
	}
	review, err := s.repo.UpdateReview(ctx, sdk.UpdateReviewOptions{ID: id, Head: head})
	if err != nil {
		return mapError(err)
	}
	fmt.Fprintf(out, "review %s now at %s (approved: %s)\n", id, shortOID(review.GetHeadCommitId()), approvedNote(review.GetApproved()))
	return nil
}

func reviewClose(ctx context.Context, out io.Writer, o *options, f *reviewFlags, args []string) error {
	id, err := reviewID("close", args)
	if err != nil {
		return err
	}
	s, err := openReview(ctx, o, f)
	if err != nil {
		return err
	}
	review, err := s.repo.CloseReview(ctx, id, f.landed)
	if err != nil {
		return mapError(err)
	}
	fmt.Fprintf(out, "review %s is %s\n", id, reviewStateName(review.GetState()))
	return nil
}

// reviewID returns the single review ID argument of a subcommand.
func reviewID(sub string, args []string) (string, error) {
	if len(args) != 1 || args[0] == "" {
		return "", usagef("review %s requires ID", sub)
	}
	return args[0], nil
}

// openReview connects to the server of the review and returns the session. The
// server comes from the workspace, or without a workspace from -address,
// -storage and -repo.
func openReview(ctx context.Context, o *options, f *reviewFlags) (*reviewSession, error) {
	t, root, err := reviewTarget(o, f)
	if err != nil {
		return nil, err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, errors.New("review: the backend cannot review changes")
	}
	return &reviewSession{repo: sb.repo, backend: backend, root: root}, nil
}

// reviewTarget returns the server and repository of a review command, and the
// workspace root. They come from the workspace when there is one. Without a
// workspace they come from -address, -storage and -repo, and the root is empty.
// Inside a workspace the repository is the one of the workspace, so -repo must
// name that repository or be left out.
func reviewTarget(o *options, f *reviewFlags) (target, string, error) {
	root, rootErr := workspaceRoot(o)
	if rootErr == nil {
		meta, metaErr := workspace.ReadMeta(root)
		if metaErr == nil {
			if f.repo != "" && f.repo != meta.Repo {
				return target{}, "", usagef("review -repo %q is not the repository of the workspace (%q): leave -repo out inside a workspace",
					f.repo, meta.Repo)
			}
			return target{
				Address: pick(o.address, meta.Address),
				Token:   o.token,
				Storage: pick(o.storage, meta.Storage),
				Repo:    meta.Repo,
				User:    o.user,
			}, root, nil
		}
		rootErr = metaErr
	}
	if o.address == "" || f.repo == "" {
		return target{}, "", rootErr
	}
	return target{Address: o.address, Token: o.token, Storage: o.storage, Repo: f.repo, User: o.user}, "", nil
}

// headOr returns head when the command names one. Otherwise it returns the
// pending commit of the workspace: the change that graft commit has made and
// has not landed yet, which is what a review is for.
func (s *reviewSession) headOr(head string) (string, error) {
	if head != "" {
		return head, nil
	}
	if s.root == "" {
		return "", usagef("review without a workspace needs --head C")
	}
	ws, err := workspace.Open(s.root, s.backend)
	if err != nil {
		return "", err
	}
	pending := ws.Summary().Pending
	switch {
	case pending == nil:
		return "", usagef("the workspace has no pending commit to review: run graft commit first, or pass --head C")
	case pending.Landed != "":
		return "", usagef("the pending commit already landed: pass --head C to review another commit")
	}
	return pending.Commit, nil
}

// writeReviewJSON writes r as one JSON object on one line.
func writeReviewJSON(out io.Writer, r *gitalypb.Review) error {
	line, err := protojson.Marshal(r)
	if err != nil {
		return err
	}
	_, err = out.Write(append(line, '\n'))
	return err
}

// formatReview writes a review for humans: its header, the classes of its
// files, the approval rules, the verdicts and the comments. Every string comes
// from other clients or from file names, so each one goes through displayText.
func formatReview(out io.Writer, r *gitalypb.Review) {
	fmt.Fprintf(out, "review %s (%s): %s\n", displayText(r.GetId()), reviewStateName(r.GetState()), displayText(string(r.GetTitle())))
	fmt.Fprintf(out, "target %s  base %s  head %s  author %s\n", displayText(string(r.GetTargetBranch())),
		shortOID(r.GetBaseCommitId()), shortOID(r.GetHeadCommitId()), displayText(r.GetAuthor()))
	if d := strings.TrimSpace(string(r.GetDescription())); d != "" {
		fmt.Fprintf(out, "\n%s\n", displayLines(d, ""))
	}
	fmt.Fprintf(out, "approved: %s\n", approvedNote(r.GetApproved()))

	if len(r.GetFiles()) > 0 {
		fmt.Fprintln(out, "files:")
		for _, f := range r.GetFiles() {
			fmt.Fprintf(out, "  %-12s %s\n", reviewClassName(f.GetClass()), displayText(string(f.GetPath())))
		}
	}
	if len(r.GetRequirements()) > 0 {
		fmt.Fprintln(out, "requirements:")
		for _, req := range r.GetRequirements() {
			state := "unsatisfied"
			if req.GetSatisfied() {
				state = "satisfied"
			}
			fmt.Fprintf(out, "  %s: %d of %d approvals (%s)\n", displayText(req.GetRule()), req.GetApprovals(), req.GetRequiredApprovals(), state)
		}
	}
	if len(r.GetVerdicts()) > 0 {
		fmt.Fprintln(out, "verdicts:")
		for _, v := range r.GetVerdicts() {
			current := ""
			if !v.GetCurrent() {
				current = " (superseded)"
			}
			fmt.Fprintf(out, "  %s by %s on %s%s", reviewVerdictName(v.GetKind()), displayText(v.GetActor()), shortOID(v.GetCommitId()), current)
			if body := strings.TrimSpace(string(v.GetBody())); body != "" {
				fmt.Fprintf(out, ": %s", displayLines(body, "    "))
			}
			fmt.Fprintln(out)
		}
	}
	if len(r.GetComments()) > 0 {
		fmt.Fprintln(out, "comments:")
		for _, c := range r.GetComments() {
			location := ""
			if p := string(c.GetPath()); p != "" {
				location = " " + displayText(p)
				if c.GetLine() > 0 {
					location += fmt.Sprintf(":%d", c.GetLine())
				}
			}
			fmt.Fprintf(out, "  %s%s on %s: %s\n", displayText(c.GetActor()), location, shortOID(c.GetCommitId()), displayLines(string(c.GetBody()), "    "))
		}
	}
}

// displayLines returns s with displayText applied to each line, so that the
// newlines of s stay newlines. Every line after the first starts with indent, so
// a multi-line text from another client reads as part of its own record and
// cannot pass for a record of its own.
func displayLines(s, indent string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = displayText(line)
	}
	return strings.Join(lines, "\n"+indent)
}

func reviewStateName(s gitalypb.Review_State) string {
	switch s {
	case gitalypb.Review_STATE_LANDED:
		return "landed"
	case gitalypb.Review_STATE_CLOSED:
		return "closed"
	default:
		return "open"
	}
}

func reviewVerdictName(k gitalypb.ReviewVerdict_Kind) string {
	if k == gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES {
		return "changes requested"
	}
	return "approved"
}

// reviewClassName is the name of a file class as shown to users and as used in
// the approval rules of the configuration.
func reviewClassName(c gitalypb.ReviewFile_ChangeClass) string {
	switch c {
	case gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY:
		return "comment_only"
	case gitalypb.ReviewFile_CHANGE_CLASS_DOCS:
		return "docs"
	case gitalypb.ReviewFile_CHANGE_CLASS_TESTS:
		return "tests"
	case gitalypb.ReviewFile_CHANGE_CLASS_GENERATED:
		return "generated"
	case gitalypb.ReviewFile_CHANGE_CLASS_CONFIG:
		return "config"
	default:
		return "code"
	}
}

func approvedNote(approved bool) string {
	if approved {
		return "yes"
	}
	return "no"
}
