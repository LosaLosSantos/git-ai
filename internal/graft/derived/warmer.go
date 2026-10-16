package derived

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/log"
)

// WarmEnv is what an eager warmer gets to compute its kind for a batch of commits.
type WarmEnv struct {
	Logger log.Logger
	Repo   *localrepo.Repo
	Reader catfile.ObjectContentReader
	// Store holds the values. Warmers write them with KindStore.Put.
	Store *Store
}

// WarmFunc computes and stores a kind for the given commits. It is called from the
// background workers of a Pool, possibly for several repositories at once, and it
// must be safe for concurrent use. A returned error only fails the warmer for this
// batch; the other warmers still run.
type WarmFunc func(ctx context.Context, env WarmEnv, commits []git.ObjectID) error

var warmers = struct {
	sync.RWMutex
	byName map[string]WarmFunc
}{byName: make(map[string]WarmFunc)}

// RegisterWarmer registers the eager warmer of the kind name. It is called from the
// init function of the package that defines the kind. The name is the store kind
// of the values. RegisterWarmer panics when the name is invalid or already
// registered, because that is a programming error.
func RegisterWarmer(name string, fn WarmFunc) {
	if !kindRegexp.MatchString(name) {
		panic(fmt.Sprintf("invalid derived warmer name %q", name))
	}
	if fn == nil {
		panic(fmt.Sprintf("nil derived warmer %q", name))
	}

	warmers.Lock()
	defer warmers.Unlock()

	if _, ok := warmers.byName[name]; ok {
		panic(fmt.Sprintf("derived warmer %q is already registered", name))
	}
	warmers.byName[name] = fn
}

// Warmers returns the names of the registered warmers, sorted.
func Warmers() []string {
	warmers.RLock()
	defer warmers.RUnlock()

	names := make([]string, 0, len(warmers.byName))
	for name := range warmers.byName {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// lookupWarmer returns the warmer registered under name.
func lookupWarmer(name string) (WarmFunc, bool) {
	warmers.RLock()
	defer warmers.RUnlock()

	fn, ok := warmers.byName[name]
	return fn, ok
}
