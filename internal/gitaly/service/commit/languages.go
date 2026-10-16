package commit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/linguist"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

var errAmbigRef = errors.New("ambiguous reference")

func (s *server) validateCommitLanguagesRequest(ctx context.Context, req *gitalypb.CommitLanguagesRequest) error {
	if err := s.locator.ValidateRepository(ctx, req.GetRepository()); err != nil {
		return err
	}
	if err := git.ValidateRevision(req.GetRevision(), git.AllowEmptyRevision()); err != nil {
		return err
	}
	return nil
}

func (s *server) CommitLanguages(ctx context.Context, req *gitalypb.CommitLanguagesRequest) (*gitalypb.CommitLanguagesResponse, error) {
	if err := s.validateCommitLanguagesRequest(ctx, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(req.GetRepository())

	revision := string(req.GetRevision())
	if revision == "" {
		defaultBranch, err := repo.GetDefaultBranch(ctx)
		if err != nil {
			return nil, err
		}
		revision = defaultBranch.String()
	}

	commitID, err := s.lookupRevision(ctx, repo, revision)
	if err != nil {
		return nil, structerr.NewInternal("looking up revision: %w", err)
	}

	stats, err := linguist.New(s.cfg, s.logger, s.catfileCache, repo).Stats(ctx, git.ObjectID(commitID))
	if err != nil {
		return nil, structerr.NewInternal("language stats: %w", err)
	}

	// CommitLanguages is classified as a mutator because it persists the computed language
	// statistics into a cache file. When running behind Praefect this means a transaction is
	// registered and every participating node is expected to cast a vote. The result is fully
	// determined by the commit being analyzed, which is identical on all replicas, so we cast a
	// single deterministic vote on the resolved commit ID. Without this vote Praefect sees a
	// "0/N votes" result and needlessly marks every replica as outdated, queueing replication on
	// every call.
	if err := transaction.VoteOnContext(ctx, s.txManager, voting.VoteFromData([]byte(commitID)), voting.Committed); err != nil {
		return nil, structerr.NewAborted("voting on commit languages: %w", err)
	}

	resp := &gitalypb.CommitLanguagesResponse{}
	if len(stats) == 0 {
		return resp, nil
	}

	total := uint64(0)
	for _, count := range stats {
		total += count
	}

	if total == 0 {
		return nil, structerr.NewInternal("linguist stats added up to zero: %v", stats)
	}

	for lang, count := range stats {
		name, languageID, err := linguist.ResolveLanguage(lang)
		if err != nil {
			s.logger.WithField("language", lang).WarnContext(ctx, "skipping unresolvable language in stats")
			continue
		}

		l := &gitalypb.CommitLanguagesResponse_Language{
			Name:       name,
			Share:      float32(100*count) / float32(total),
			Color:      linguist.Color(name),
			LanguageId: int64(languageID),
			Bytes:      count,
		}
		resp.Languages = append(resp.Languages, l)
	}

	sort.Sort(languageSorter(resp.GetLanguages()))

	return resp, nil
}

type languageSorter []*gitalypb.CommitLanguagesResponse_Language

func (ls languageSorter) Len() int           { return len(ls) }
func (ls languageSorter) Swap(i, j int)      { ls[i], ls[j] = ls[j], ls[i] }
func (ls languageSorter) Less(i, j int) bool { return ls[i].GetShare() > ls[j].GetShare() }

func (s *server) lookupRevision(ctx context.Context, repo gitcmd.RepositoryExecutor, revision string) (string, error) {
	rev, err := s.checkRevision(ctx, repo, revision)
	if err != nil {
		if errors.Is(err, errAmbigRef) {
			fullRev, err := s.disambiguateRevision(ctx, repo, revision)
			if err != nil {
				return "", err
			}

			rev, err = s.checkRevision(ctx, repo, fullRev)
			if err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}

	return rev, nil
}

func (s *server) checkRevision(ctx context.Context, repo gitcmd.RepositoryExecutor, revision string) (string, error) {
	var stdout, stderr bytes.Buffer

	revParse, err := repo.Exec(ctx,
		gitcmd.Command{Name: "rev-parse", Args: []string{revision}},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	)
	if err != nil {
		return "", err
	}

	if err = revParse.Wait(); err != nil {
		errMsg := strings.Split(stderr.String(), "\n")[0]
		return "", fmt.Errorf("%w: %v", err, errMsg)
	}

	if strings.HasSuffix(stderr.String(), "refname '"+revision+"' is ambiguous.\n") {
		return "", errAmbigRef
	}

	return text.ChompBytes(stdout.Bytes()), nil
}

func (s *server) disambiguateRevision(ctx context.Context, repo gitcmd.RepositoryExecutor, revision string) (string, error) {
	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:  "for-each-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--format=%(refname)"}},
		Args:  []string{"**/" + revision},
	}, gitcmd.WithSetupStdout())
	if err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(cmd)
	for scanner.Scan() {
		refName := scanner.Text()

		if strings.HasPrefix(refName, "refs/heads") {
			return refName, nil
		}
	}

	return "", fmt.Errorf("branch %v not found", revision)
}
