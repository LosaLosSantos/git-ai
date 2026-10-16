package catfile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/helper"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"gitlab.com/gitlab-org/labkit/correlation"
	"go.opentelemetry.io/otel/attribute"
)

const (
	// defaultBatchfileTTL is the default ttl for batch files to live in the cache
	defaultBatchfileTTL = 10 * time.Second

	defaultEvictionInterval = 1 * time.Second

	// The default maximum number of cache entries
	defaultMaxLen = 100
)

// Cache is a cache for git-cat-file(1) processes.
type Cache interface {
	// ObjectReader either creates a new object reader or returns a cached one for the given
	// repository.
	ObjectReader(context.Context, gitcmd.RepositoryExecutor) (ObjectContentReader, func(), error)
	// ObjectReaderWithoutMailmap is similar to ObjectReader but with mailmap disabled.
	ObjectReaderWithoutMailmap(context.Context, gitcmd.RepositoryExecutor) (ObjectContentReader, func(), error)
	// ObjectInfoReader either creates a new object info reader or returns a cached one for the
	// given repository.
	ObjectInfoReader(context.Context, gitcmd.RepositoryExecutor) (ObjectInfoReader, func(), error)
	// Evict evicts all cached processes from the cache.
	Evict()
}

type cacheable interface {
	isClosed() bool
	isDirty() bool
	close()
}

// ProcessCache entries always get added to the back of the list. If the
// list gets too long, we evict entries from the front of the list. When
// an entry gets added it gets an expiry time based on a fixed TTL. A
// monitor goroutine periodically evicts expired entries.
type ProcessCache struct {
	// ttl is the fixed ttl for cache entries
	ttl time.Duration
	// monitorTicker is the ticker used for the monitoring Goroutine.
	monitorTicker helper.Ticker
	monitorDone   chan struct{}

	objectReaders               processes
	objectReadersWithoutMailmap processes

	catfileCacheCounter     *prometheus.CounterVec
	currentCatfileProcesses prometheus.Gauge
	totalCatfileProcesses   prometheus.Counter
	catfileLookupCounter    *prometheus.CounterVec
	catfileCacheMembers     *prometheus.GaugeVec
}

// NewCache creates a new catfile process cache.
func NewCache(cfg config.Cfg) *ProcessCache {
	return newCache(defaultBatchfileTTL, cfg.Git.CatfileCacheSize, helper.NewTimerTicker(defaultEvictionInterval))
}

func newCache(ttl time.Duration, maxLen int, monitorTicker helper.Ticker) *ProcessCache {
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}

	processCache := &ProcessCache{
		ttl: ttl,
		objectReaders: processes{
			maxLen: maxLen,
		},
		objectReadersWithoutMailmap: processes{
			maxLen: maxLen,
		},
		catfileCacheCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gitaly_catfile_cache_total",
				Help: "Counter of catfile cache hit/miss",
			},
			[]string{"type"},
		),
		currentCatfileProcesses: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "gitaly_catfile_processes",
				Help: "Gauge of active catfile processes",
			},
		),
		totalCatfileProcesses: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "gitaly_catfile_processes_total",
				Help: "Counter of catfile processes",
			},
		),
		catfileLookupCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gitaly_catfile_lookups_total",
				Help: "Git catfile lookups by object type",
			},
			[]string{"type"},
		),
		catfileCacheMembers: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gitaly_catfile_cache_members",
				Help: "Gauge of catfile cache members by process type",
			},
			[]string{"type"},
		),
		monitorTicker: monitorTicker,
		monitorDone:   make(chan struct{}),
	}

	go processCache.monitor()
	return processCache
}

// Describe describes all metrics exposed by ProcessCache.
func (c *ProcessCache) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, descs)
}

// Collect collects all metrics exposed by ProcessCache.
func (c *ProcessCache) Collect(metrics chan<- prometheus.Metric) {
	c.catfileCacheCounter.Collect(metrics)
	c.currentCatfileProcesses.Collect(metrics)
	c.totalCatfileProcesses.Collect(metrics)
	c.catfileLookupCounter.Collect(metrics)
	c.catfileCacheMembers.Collect(metrics)
}

func (c *ProcessCache) monitor() {
	c.monitorTicker.Reset()

	for {
		select {
		case <-c.monitorTicker.C():
			c.objectReaders.EnforceTTL(time.Now())
			c.objectReadersWithoutMailmap.EnforceTTL(time.Now())
			c.monitorTicker.Reset()
		case <-c.monitorDone:
			return
		}

		c.reportCacheMembers()
	}
}

// Stop stops the monitoring Goroutine and evicts all cached processes. This must only be called
// once.
func (c *ProcessCache) Stop() {
	c.monitorTicker.Stop()
	close(c.monitorDone)
	c.Evict()
}

func (c *ProcessCache) objectReaderRaw(ctx context.Context, repo gitcmd.RepositoryExecutor, withoutMailmap bool) (ObjectContentReader, func(), error) {
	var cached cacheable
	var err error
	var cancel func()

	if withoutMailmap {
		cached, cancel, err = c.getOrCreateProcess(ctx, repo, &c.objectReadersWithoutMailmap, func(ctx context.Context) (cacheable, error) {
			return newObjectReader(ctx, repo, c.catfileLookupCounter, WithoutMailmap())
		}, "catfile.ObjectReaderWithoutMailmap")
	} else {
		cached, cancel, err = c.getOrCreateProcess(ctx, repo, &c.objectReaders, func(ctx context.Context) (cacheable, error) {
			return newObjectReader(ctx, repo, c.catfileLookupCounter)
		}, "catfile.ObjectReader")
	}
	if err != nil {
		return nil, nil, err
	}

	objectReader, ok := cached.(ObjectContentReader)
	if !ok {
		return nil, nil, fmt.Errorf("expected object reader, got %T", cached)
	}

	return objectReader, cancel, nil
}

// ObjectReader creates a new ObjectReader process for the given repository.
func (c *ProcessCache) ObjectReader(ctx context.Context, repo gitcmd.RepositoryExecutor) (ObjectContentReader, func(), error) {
	return c.objectReaderRaw(ctx, repo, false)
}

// ObjectReaderWithoutMailmap creates a new ObjectReader but without 'mailmap' support.
func (c *ProcessCache) ObjectReaderWithoutMailmap(ctx context.Context, repo gitcmd.RepositoryExecutor) (ObjectContentReader, func(), error) {
	return c.objectReaderRaw(ctx, repo, true)
}

// ObjectInfoReader creates a new ObjectInfoReader process for the given repository.
func (c *ProcessCache) ObjectInfoReader(ctx context.Context, repo gitcmd.RepositoryExecutor) (ObjectInfoReader, func(), error) {
	var cached cacheable
	var err error
	var cancel func()

	cached, cancel, err = c.getOrCreateProcess(ctx, repo, &c.objectReaders, func(ctx context.Context) (cacheable, error) {
		return newObjectReader(ctx, repo, c.catfileLookupCounter)
	}, "catfile.ObjectReader")
	if err != nil {
		return nil, nil, err
	}

	objectInfoReader, ok := cached.(ObjectInfoReader)
	if !ok {
		return nil, nil, fmt.Errorf("expected object info reader, got %T", cached)
	}

	return objectInfoReader, cancel, nil
}

// Calculate the nearest 5-minute interval. For our cache key, we want to
// enforce expiry after 5 minutes.
func roundToNearestFiveMinute(t time.Time) int {
	return ((t.Minute() / 5) + 1) * 5
}

func (c *ProcessCache) getOrCreateProcess(
	ctx context.Context,
	repo storage.Repository,
	processes *processes,
	create func(context.Context) (cacheable, error),
	spanName string,
) (_ cacheable, _ func(), returnedErr error) {
	defer c.reportCacheMembers()

	span, ctx := tracing.StartSpanIfHasParent(ctx, spanName, nil)
	defer span.End()

	cacheKey, isCacheable := newCacheKey(fmt.Sprintf("%d", roundToNearestFiveMinute(time.Now())), repo)

	// When the repository uses the MVCC reference backend, the manifest the
	// process was spawned under pins the view of the repository it can serve.
	// It must therefore be part of the cache key so that a process is never
	// handed to an RPC operating on a different manifest (repository view).
	if mvccCache := mvcc.CacheFromContext(ctx); mvccCache != nil {
		if err := mvccCache.WaitUntilReady(ctx); err != nil {
			return nil, nil, fmt.Errorf("waiting for MVCC cache: %w", err)
		}

		// If the RPC is a read-only one, it is safe to cache the catfile process
		// as the manifest hash will be the same for the whole duration of the RPC
		// and will be part of the cache key. Any other read RPC with the same
		// manifest hash (same key) will be able to reuse the process safely.
		//
		// For non-read-only RPC, given that the git command is run with `GIT_MVCC_MANIFEST_PATH`,
		// any other RPC recycling the same process of another non-read-only RPC will end up
		// sharing same the view of the repository of the RPC that spawned the process, which
		// will result in corrupted reads and writes, as this view might be stale by the moment
		// the process is reused. For this reason, for simplicity, when using mvcc, we only
		// cache processes for read-only RPCs.
		pointer := mvccCache.BaseManifestPointer()
		if mvccCache.ReadOnly() && pointer != "" {
			cacheKey.mvccManifest = pointer
		} else {
			isCacheable = false
		}
	}

	if isCacheable {
		// We only try to look up cached processes in case it is cacheable, which requires a
		// session ID. This is mostly done such that git-cat-file(1) processes from one user
		// cannot interfere with those from another user. The main intent is to disallow
		// trivial denial of service attacks against other users in case it is possible to
		// poison the cache with broken git-cat-file(1) processes.

		if entry, ok := processes.Checkout(cacheKey); ok {
			c.catfileCacheCounter.WithLabelValues("hit").Inc()
			span.SetAttributes(attribute.Bool("hit", true))
			return entry.value, func() {
				c.returnToCache(processes, cacheKey, entry.value, entry.cancel)
			}, nil
		}

		c.catfileCacheCounter.WithLabelValues("miss").Inc()
		span.SetAttributes(attribute.Bool("hit", false))

		// When cache misses, a new process is created. This process may be re-used later.
		// In that case, the lifecycle of the process is stretched across multiple
		// gorountines. We should not attribute the span of this shared process to the
		// current trace.
		ctx = tracing.DiscardSpanInContext(ctx)
		// We have not found any cached process, so we need to create a new one. In this
		// case, we need to detach the process from the current context such that it does
		// not get killed when the parent context is cancelled.
		//
		// Note that we explicitly retain feature flags here, which means that cached
		// processes may retain flags for some time which have been changed meanwhile. While
		// not ideal, it feels better compared to just ignoring feature flags altogether.
		// The latter would mean that we cannot use flags in the catfile code, but more
		// importantly we also wouldn't be able to use feature-flagged Git version upgrades
		// for catfile processes.
		ctx = context.WithoutCancel(ctx)
		// We have to decorrelate the process from the current context given that it
		// may potentially be reused across different RPC calls.
		ctx = correlation.ContextWithCorrelation(ctx, "")
	}

	// Create a new cancellable process context such that we can kill it on demand. If it's a
	// cached process, then it will only be killed when the cache evicts the entry because we
	// detached the background further up. If it's an uncached value, we either kill it manually
	// or via the RPC context's cancellation function.
	ctx, cancelProcessContext := context.WithCancel(ctx)
	defer func() {
		if returnedErr != nil {
			cancelProcessContext()
		}
	}()

	process, err := create(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		// If we somehow fail after creating a new process, then we want to kill spawned
		// processes right away.
		if returnedErr != nil {
			process.close()
		}
	}()

	c.totalCatfileProcesses.Inc()
	c.currentCatfileProcesses.Inc()

	// Note that we must make sure that `cancel` and `closeProcess` are two different variables.
	// Otherwise if we passed `cancel` to `returnToCache` and then set `cancel` itself to the
	// function that calls it we would accidentally call ourselves and end up with a segfault.
	closeProcess := func() {
		cancelProcessContext()
		process.close()
		c.currentCatfileProcesses.Dec()
	}

	cancel := closeProcess
	if isCacheable {
		// If the process is cacheable, then we want to put the process into the cache when
		// the current outer context is done.
		cancel = func() {
			c.returnToCache(processes, cacheKey, process, closeProcess)
		}
	}

	return process, sync.OnceFunc(cancel), nil
}

func (c *ProcessCache) reportCacheMembers() {
	c.catfileCacheMembers.WithLabelValues("object_reader").Set(float64(c.objectReaders.EntryCount()))
	c.catfileCacheMembers.WithLabelValues("object_reader_without_mailmap").Set(float64(c.objectReadersWithoutMailmap.EntryCount()))
}

// Evict evicts all cached processes from the cache.
func (c *ProcessCache) Evict() {
	c.objectReaders.Evict()
	c.objectReadersWithoutMailmap.Evict()
}

func (c *ProcessCache) returnToCache(p *processes, cacheKey key, value cacheable, cancel func()) {
	defer func() {
		c.reportCacheMembers()
	}()

	if value == nil || value.isClosed() {
		cancel()
		return
	}

	if value.isDirty() {
		cancel()
		c.catfileCacheCounter.WithLabelValues("dirty").Inc()
		value.close()
		return
	}

	if replaced := p.Add(cacheKey, value, time.Now().Add(c.ttl), cancel); replaced {
		c.catfileCacheCounter.WithLabelValues("duplicate").Inc()
	}
}

type key struct {
	sessionID string
	// mvccManifest is empty for repositories that do not use MVCC backend
	mvccManifest string
	repoStorage  string
	repoRelPath  string
	repoObjDir   string
	repoAltDir   string
}

func newCacheKey(sessionID string, repo storage.Repository) (key, bool) {
	if sessionID == "" {
		return key{}, false
	}

	return key{
		sessionID:   sessionID,
		repoStorage: repo.GetStorageName(),
		repoRelPath: repo.GetRelativePath(),
		repoObjDir:  repo.GetGitObjectDirectory(),
		repoAltDir:  strings.Join(repo.GetGitAlternateObjectDirectories(), ","),
	}, true
}

type entry struct {
	key
	value  cacheable
	expiry time.Time
	cancel func()
}

type processes struct {
	maxLen int

	entriesMutex sync.Mutex
	entries      []*entry
}

// Add adds a key, value pair to p. If there are too many keys in p
// already add will evict old keys until the length is OK again.
func (p *processes) Add(k key, value cacheable, expiry time.Time, cancel func()) bool {
	var evicted []*entry

	p.entriesMutex.Lock()

	replacedExisting := false
	if i, ok := p.lookup(k); ok {
		evicted = append(evicted, p.delete(i))
		replacedExisting = true
	}

	ent := &entry{
		key:    k,
		value:  value,
		expiry: expiry,
		cancel: cancel,
	}
	p.entries = append(p.entries, ent)

	for len(p.entries) > p.maxLen {
		evicted = append(evicted, p.delete(0))
	}

	p.entriesMutex.Unlock()

	closeEntries(evicted)

	return replacedExisting
}

// Checkout removes a value from p. After use the caller can re-add the value with p.Add.
func (p *processes) Checkout(k key) (*entry, bool) {
	p.entriesMutex.Lock()
	defer p.entriesMutex.Unlock()

	i, ok := p.lookup(k)
	if !ok {
		return nil, false
	}

	return p.delete(i), true
}

// EnforceTTL evicts all entries older than now, assuming the entry
// expiry times are increasing.
func (p *processes) EnforceTTL(now time.Time) {
	var expired []*entry

	p.entriesMutex.Lock()
	for len(p.entries) > 0 && now.After(p.head().expiry) {
		expired = append(expired, p.delete(0))
	}
	p.entriesMutex.Unlock()

	closeEntries(expired)
}

// Evict evicts all cached processes from the cache.
func (p *processes) Evict() {
	p.entriesMutex.Lock()
	evicted := p.entries
	p.entries = nil
	p.entriesMutex.Unlock()

	closeEntries(evicted)
}

// EntryCount returns the number of cached entries. This function will locks the ProcessCache to
// avoid races.
func (p *processes) EntryCount() int {
	p.entriesMutex.Lock()
	defer p.entriesMutex.Unlock()
	return len(p.entries)
}

func (p *processes) head() *entry { return p.entries[0] }

func (p *processes) lookup(k key) (int, bool) {
	for i, ent := range p.entries {
		if ent.key == k {
			return i, true
		}
	}

	return -1, false
}

// delete removes the entry at index i from p and returns it. It does not close
// the process; the caller must do that via closeEntries after releasing
// entriesMutex.
func (p *processes) delete(i int) *entry {
	ent := p.entries[i]
	p.entries = slices.Delete(p.entries, i, i+1)
	return ent
}

// closeEntries cancels and closes the given evicted cache entries. It must be
// called without holding entriesMutex: close() waits for the git subprocess to
// exit, which can block indefinitely if the process is wedged. Holding the lock
// across that wait would stall every other cache operation and, during shutdown,
// wedge ProcessCache.Stop.
func closeEntries(entries []*entry) {
	for _, ent := range entries {
		// We first cancel the context such that the process gets signaled to
		// terminate. Calling `close()` first may lead to a deadlock given that it
		// waits for the process to exit, which may not happen if it hangs writing
		// data to stdout.
		ent.cancel()
		ent.value.close()
	}
}
