package linguist

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitattributes"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gitpipe"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/go-enry/go-enry/v2"
)

// ByteCountPerLanguage represents a counter value (bytes) per language.
type ByteCountPerLanguage map[string]uint64

// Instance is a holder of the defined in the system language settings.
type Instance struct {
	cfg          config.Cfg
	logger       log.Logger
	catfileCache catfile.Cache
	repo         *localrepo.Repo
}

// New creates a new instance that can be used to calculate language stats for
// the given repo.
func New(cfg config.Cfg, logger log.Logger, catfileCache catfile.Cache, repo *localrepo.Repo) *Instance {
	return &Instance{
		cfg:          cfg,
		logger:       logger,
		catfileCache: catfileCache,
		repo:         repo,
	}
}

// Color returns the color Linguist has assigned to language.
func Color(language string) string {
	if color := enry.GetColor(language); color != "#cccccc" {
		return color
	}

	colorSha := sha256.Sum256([]byte(language))
	return fmt.Sprintf("#%x", colorSha[0:3])
}

// ResolveLanguage returns the canonical language name and ID from
// https://github.com/github-linguist/linguist/blob/main/lib/linguist/languages.yml,
// resolving aliases if needed. Returns an error for unrecognized names.
func ResolveLanguage(language string) (string, int, error) {
	if id, ok := enry.GetLanguageID(language); ok {
		return language, id, nil
	}
	if canonical, ok := enry.GetLanguageByAlias(language); ok {
		if id, ok := enry.GetLanguageID(canonical); ok {
			return canonical, id, nil
		}
	}
	return language, -1, fmt.Errorf("linguist language_id not found: %s", language)
}

// hasStaleLanguageNames reports whether languages contains any name not
// recognized as canonical by go-enry, indicating a stale or corrupt cache.
func hasStaleLanguageNames(languages ByteCountPerLanguage) bool {
	for lang := range languages {
		if _, ok := enry.GetLanguageID(lang); !ok {
			return true
		}
	}
	return false
}

// Stats returns the repository's language statistics.
func (inst *Instance) Stats(ctx context.Context, commitID git.ObjectID) (ByteCountPerLanguage, error) {
	stats, err := initLanguageStats(ctx, inst.repo)
	if err != nil {
		inst.logger.WithError(err).InfoContext(ctx, "linguist load from cache")
	}
	staleNames := stats.CommitID != "" && hasStaleLanguageNames(stats.Totals)
	if stats.CommitID == commitID && !staleNames {
		return stats.Totals, nil
	}

	objectReader, cancel, err := inst.catfileCache.ObjectReader(ctx, inst.repo)
	if err != nil {
		return nil, fmt.Errorf("linguist create object reader: %w", err)
	}
	defer cancel()

	checkAttr, finishAttr, err := gitattributes.CheckAttr(ctx, inst.repo, commitID.Revision(), linguistAttrs)
	if err != nil {
		return nil, fmt.Errorf("linguist create check attr: %w", err)
	}
	defer finishAttr()

	var revlistIt gitpipe.RevisionIterator

	full, err := inst.needsFullRecalculation(ctx, stats.CommitID, commitID)
	if err != nil {
		return nil, fmt.Errorf("linguist cannot determine full recalculation: %w", err)
	}

	if full || staleNames {
		stats = newLanguageStats()

		skipFunc := func(result *gitpipe.RevisionResult) (bool, error) {
			f, err := newFileInstance(string(result.ObjectName), checkAttr)
			if err != nil {
				return true, fmt.Errorf("new file instance: %w", err)
			}

			// Skip files that are an excluded filetype based on filename.
			return f.IsExcluded(), nil
		}

		// Full recalculation is needed, so get all the files for the
		// commit using git-ls-tree(1).
		revlistIt = gitpipe.LsTree(ctx, inst.repo,
			commitID.String(),
			gitpipe.LsTreeWithRecursive(),
			gitpipe.LsTreeWithBlobFilter(),
			gitpipe.LsTreeWithSkip(skipFunc),
		)
	} else {
		// Stats are cached for one commit, so get the git-diff-tree(1)
		// between that commit and the one we're calculating stats for.

		hash, err := inst.repo.ObjectHash(ctx)
		if err != nil {
			return nil, fmt.Errorf("linguist: detect object hash: %w", err)
		}

		skipFunc := func(result *gitpipe.RevisionResult) (bool, error) {
			var skip bool

			// Skip files that are deleted, that are not a blob (e.g. a
			// submodule gitlink), or an excluded filetype based on filename.
			if hash.IsZeroOID(result.OID) || !result.IsBlob() {
				skip = true
			} else {
				f, err := newFileInstance(string(result.ObjectName), checkAttr)
				if err != nil {
					return false, fmt.Errorf("new file instance: %w", err)
				}
				skip = f.IsExcluded()
			}

			if skip {
				// It's a little bit of a hack to use this skip
				// function, but for every file that's deleted,
				// remove the stats.
				stats.drop(string(result.ObjectName))
				return true, nil
			}
			return false, nil
		}

		revlistIt = gitpipe.DiffTree(ctx, inst.repo,
			stats.CommitID.String(), commitID.String(),
			gitpipe.DiffTreeWithRecursive(),
			gitpipe.DiffTreeWithIgnoreSubmodules(),
			gitpipe.DiffTreeWithSkip(skipFunc),
		)
	}

	objectIt, err := gitpipe.CatfileObject(ctx, objectReader, revlistIt)
	if err != nil {
		return nil, fmt.Errorf("linguist gitpipe: %w", err)
	}

	for objectIt.Next() {
		object := objectIt.Result()
		filename := string(object.ObjectName)

		f, err := newFileInstance(filename, checkAttr)
		if err != nil {
			return nil, fmt.Errorf("linguist new file instance: %w", err)
		}

		lang, size, err := f.DetermineStats(object)
		if err != nil {
			return nil, fmt.Errorf("linguist determine stats: %w", err)
		}

		// Ensure object content is completely consumed
		if _, err := io.Copy(io.Discard, object); err != nil {
			return nil, fmt.Errorf("linguist discard excess blob: %w", err)
		}

		if len(lang) == 0 {
			stats.drop(filename)

			continue
		}

		stats.add(filename, lang, size)
	}

	if err := objectIt.Err(); err != nil {
		return nil, fmt.Errorf("linguist object iterator: %w", err)
	}

	if err := stats.save(ctx, inst.repo, commitID); err != nil {
		return nil, fmt.Errorf("linguist language stats save: %w", err)
	}

	return stats.Totals, nil
}

func (inst *Instance) needsFullRecalculation(ctx context.Context, cachedID, commitID git.ObjectID) (bool, error) {
	if cachedID == "" {
		return true, nil
	}

	// The cached commit may no longer exist after a force-push, GC/repack, or
	// repository restore/move. Peeling with `^{commit}` ensures the object is
	// specifically a commit, not just any object type (blob, tree, or tag)
	exists, err := inst.repo.HasRevision(ctx, git.Revision(cachedID.String()+"^{commit}"))
	if err != nil {
		return true, fmt.Errorf("linguist: verifying cached commit: %w", err)
	}
	if !exists {
		inst.logger.WithField("cached_commit_id", cachedID.String()).InfoContext(ctx, "linguist: cached commit no longer exists, forcing full recalculation")
		return true, nil
	}

	err = inst.repo.ExecAndWait(ctx, gitcmd.Command{
		Name:        "diff",
		Flags:       []gitcmd.Option{gitcmd.Flag{Name: "--quiet"}},
		Args:        []string{fmt.Sprintf("%v..%v", cachedID, commitID)},
		PostSepArgs: []string{".gitattributes"},
	})
	if err == nil {
		return false, nil
	}
	if code, ok := command.ExitStatus(err); ok && code == 1 {
		return true, nil
	}

	return true, fmt.Errorf("git diff .gitattributes: %w", err)
}
