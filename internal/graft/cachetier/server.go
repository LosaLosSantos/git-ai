// Package cachetier implements graft-cache, a read-through cache tier for
// Graft's ObjectService. A cache tier sits between clients and a Gitaly (or
// another cache tier) and serves immutable, content-addressed reads locally.
// Every other RPC is proxied transparently to the upstream.
//
// Trust model. The tier does not hold an access-control list of its own. Every
// ObjectService read first asks the upstream, with the caller's credentials,
// whether the repository exists and is readable (RepositoryExists). A caller the
// upstream rejects gets the upstream's error, even when the content is cached.
// Only callers that send no authorization are authenticated with the tier's
// --token, and only when one is configured. Cached content is shared by all
// callers that the upstream authorizes for the same repository. Clients must
// reach the tier only through the same trust boundary they use for the
// upstream.
//
// Quarantine and alternate object directories. A repository that reads through
// git_object_directory or git_alternate_object_directories sees objects that
// the plain repository does not, for example the objects of a rejected push. Such
// requests bypass the cache in both directions, so their objects are never
// stored and never served to plain readers.
//
// Memory. A GetObjects request loads all of its objects before it sends the
// first message. Every object it holds that is not served from the in-memory LRU
// (disk hits, upstream misses and objects taken over from another request's
// fill) counts against max-request-bytes. A request over that budget is streamed
// through the upstream without being cached. A GetTree listing is buffered only
// up to min(max-bytes, max-request-bytes). A larger listing is streamed through
// and not cached. Objects larger than the in-memory budget are not cached in
// memory.
//
// Seeding. A tier can be preloaded at start from seed files, which hold trees and
// blobs of a repository taken with WriteSnapshot (graft-cache snapshot). A seed
// file is gzip-compressed and consists of the header line "graft-cache-seed v1\n"
// followed by records. Each record is a uvarint length, then a body of:
//
//	uvarint(len(storage)) storage
//	uvarint(len(relative path)) relative path
//	uvarint(len(oid)) oid
//	one byte, the gitalypb.ObjectType
//	uvarint(size) data, where size is len(data)
//
// Every seeded object is verified against its object ID exactly like an object
// fetched from upstream. A corrupt record is skipped and counted, and a damaged
// frame ends the seed. Seeded objects are served without upstream object reads,
// but every read is still authorized by the upstream first.
package cachetier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

const (
	// TierCache is a regional tier. It persists objects to disk when a disk
	// directory is configured.
	TierCache = "cache"
	// TierDaemon is a per-node tier. It keeps objects in memory only.
	TierDaemon = "daemon"

	// DefaultDiskMaxBytes bounds the on-disk store when Config.DiskMaxBytes is unset.
	DefaultDiskMaxBytes = 10 << 30

	defaultTreePageSize = 1000
)

// DefaultMaxRequestBytes is the default byte budget of one GetObjects request
// while it buffers its misses.
const DefaultMaxRequestBytes = 64 << 20

// Config configures a cache tier server.
type Config struct {
	// Tier is TierCache or TierDaemon.
	Tier string
	// MaxBytes bounds the in-memory LRU. Required.
	MaxBytes int64
	// DiskDir enables the on-disk object store. Only valid for TierCache.
	DiskDir string
	// DiskMaxBytes bounds the on-disk store. When it is exceeded, the oldest
	// objects are evicted first. Defaults to DefaultDiskMaxBytes.
	DiskMaxBytes int64
	// Token is sent upstream on requests that carry no authorization of their own.
	// Requests that carry authorization always use the client's own.
	Token string
	// ChunkSize is the maximum data bytes per GetObjects message. It must match
	// the upstream's ObjectService.ChunkSize so that both return identically
	// chunked streams. Defaults to config.DefaultGraftChunkSize.
	ChunkSize int
	// MaxObjects bounds the object IDs per GetObjects request, exactly like the
	// upstream's ObjectService.MaxObjects. It also bounds the size of one
	// batched upstream fill, so a request needs at most one upstream call. Set
	// it to the upstream's limit. Defaults to config.DefaultGraftMaxObjects.
	MaxObjects int
	// MaxRequestBytes bounds the object data that one GetObjects request buffers
	// outside the in-memory LRU, whether it comes from the disk store, from the
	// upstream or from another request's fill. A request that would exceed it is
	// forwarded to the upstream and streamed through without being cached. A
	// GetTree listing is buffered only up to the smaller of MaxRequestBytes and
	// MaxBytes, and a larger listing is streamed through without being cached. The
	// tier's memory therefore stays bounded by MaxRequestBytes per request plus
	// MaxBytes. Defaults to DefaultMaxRequestBytes.
	MaxRequestBytes int64
	// TreePageSize is the maximum entries per GetTree message.
	TreePageSize int
	// Logger receives operational messages. Defaults to the command logger.
	Logger log.Logger
}

// Stats is a snapshot of the cache counters, served as JSON on the stats endpoint.
type Stats struct {
	Tier           string `json:"tier"`
	MaxBytes       int64  `json:"max_bytes"`
	MemoryBytes    int64  `json:"memory_bytes"`
	MemoryEntries  int    `json:"memory_entries"`
	ObjectHits     int64  `json:"object_hits"`
	ObjectMisses   int64  `json:"object_misses"`
	DiskHits       int64  `json:"disk_hits"`
	Coalesced      int64  `json:"coalesced"`
	TreeHits       int64  `json:"tree_hits"`
	TreeMisses     int64  `json:"tree_misses"`
	Evictions      int64  `json:"evictions"`
	VerifyFailures int64  `json:"verify_failures"`
	ProxiedCalls   int64  `json:"proxied_calls"`
	// HitRate is the share of this tier's cache lookups that were served locally,
	// over objects and trees: (object hits + disk hits + tree hits) divided by all
	// lookups. A coalesced object counts as a lookup that was not a hit. It is 0
	// before the first lookup.
	HitRate float64 `json:"hit_rate"`
	// ObjectHitRate is the share of object lookups served from memory or disk.
	ObjectHitRate float64 `json:"object_hit_rate"`
	// TreeHitRate is the share of GetTree listings served from memory.
	TreeHitRate float64 `json:"tree_hit_rate"`
	// SeededObjects and SeededBytes count the objects loaded from seed files.
	SeededObjects int64 `json:"seeded_objects"`
	SeededBytes   int64 `json:"seeded_bytes"`
	// SeedCorrupt counts seed records that were skipped because they were damaged.
	SeedCorrupt int64 `json:"seed_corrupt"`
}

// Server is the cache tier. It implements ObjectService and is wired into a
// gRPC server with NewGRPCServer, which also proxies all other services.
type Server struct {
	gitalypb.UnimplementedObjectServiceServer

	upstreamConn *grpc.ClientConn
	upstream     gitalypb.ObjectServiceClient
	repos        gitalypb.RepositoryServiceClient
	token        string
	tier         string
	maxBytes     int64
	chunkSize    int64
	maxObjects   int
	// maxRequestBytes bounds the data one request buffers while it loads misses.
	maxRequestBytes int64
	treePageSize    int
	logger          log.Logger

	memory *lru[cacheKey, *cacheEntry]
	disk   *diskStore

	// mu guards inflight and treeInflight. It is never held while doing I/O.
	mu           sync.Mutex
	inflight     map[objectKey]*flight
	treeInflight map[cacheKey]*treeFlight

	objectHits     atomic.Int64
	objectMisses   atomic.Int64
	diskHits       atomic.Int64
	coalesced      atomic.Int64
	treeHits       atomic.Int64
	treeMisses     atomic.Int64
	evictions      atomic.Int64
	verifyFailures atomic.Int64
	proxiedCalls   atomic.Int64
	seededObjects  atomic.Int64
	seededBytes    atomic.Int64
	seedCorrupt    atomic.Int64

	registry *prometheus.Registry
}

// NewServer returns a cache tier that reads from upstream. The caller owns the
// connection and must keep it open for the lifetime of the server.
func NewServer(upstream *grpc.ClientConn, cfg Config) (*Server, error) {
	if upstream == nil {
		return nil, errors.New("upstream connection is required")
	}
	if cfg.Tier != TierCache && cfg.Tier != TierDaemon {
		return nil, fmt.Errorf("unknown tier %q: must be %q or %q", cfg.Tier, TierCache, TierDaemon)
	}
	if cfg.MaxBytes <= 0 {
		return nil, errors.New("max bytes must be positive")
	}
	if cfg.DiskDir != "" && cfg.Tier != TierCache {
		return nil, errors.New("a disk directory is only supported by the cache tier")
	}

	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = config.DefaultGraftChunkSize
	}
	diskMaxBytes := cfg.DiskMaxBytes
	if diskMaxBytes <= 0 {
		diskMaxBytes = DefaultDiskMaxBytes
	}
	maxObjects := cfg.MaxObjects
	if maxObjects <= 0 {
		maxObjects = config.DefaultGraftMaxObjects
	}
	maxRequestBytes := cfg.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = DefaultMaxRequestBytes
	}
	treePageSize := cfg.TreePageSize
	if treePageSize <= 0 {
		treePageSize = defaultTreePageSize
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.ConfigureCommand()
	}

	s := &Server{
		upstreamConn:    upstream,
		upstream:        gitalypb.NewObjectServiceClient(upstream),
		repos:           gitalypb.NewRepositoryServiceClient(upstream),
		token:           cfg.Token,
		tier:            cfg.Tier,
		maxBytes:        cfg.MaxBytes,
		chunkSize:       int64(chunkSize),
		maxObjects:      maxObjects,
		maxRequestBytes: maxRequestBytes,
		treePageSize:    treePageSize,
		logger:          logger,
		inflight:        make(map[objectKey]*flight),
		treeInflight:    make(map[cacheKey]*treeFlight),
		registry:        prometheus.NewRegistry(),
	}

	s.memory = newLRU[cacheKey, *cacheEntry](cfg.MaxBytes,
		func(e *cacheEntry) int64 { return e.size },
		func() { s.evictions.Add(1) },
	)

	if cfg.DiskDir != "" {
		disk, err := newDiskStore(cfg.DiskDir, diskMaxBytes)
		if err != nil {
			return nil, err
		}
		s.disk = disk
	}

	s.registerMetrics()
	return s, nil
}

func (s *Server) registerMetrics() {
	counter := func(name, help string, value *atomic.Int64) {
		s.registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, func() float64 {
			return float64(value.Load())
		}))
	}

	counter("graft_cache_object_hits_total", "Objects served from the in-memory LRU.", &s.objectHits)
	counter("graft_cache_object_misses_total", "Objects not found in the in-memory LRU.", &s.objectMisses)
	counter("graft_cache_disk_hits_total", "Objects served from the on-disk store.", &s.diskHits)
	counter("graft_cache_coalesced_total", "Object loads that joined an in-flight load.", &s.coalesced)
	counter("graft_cache_tree_hits_total", "GetTree listings served from cache.", &s.treeHits)
	counter("graft_cache_tree_misses_total", "GetTree listings fetched from upstream.", &s.treeMisses)
	counter("graft_cache_evictions_total", "Entries evicted from the in-memory LRU.", &s.evictions)
	counter("graft_cache_verify_failures_total", "Upstream objects whose content did not match their ID.", &s.verifyFailures)
	counter("graft_cache_proxied_calls_total", "RPCs proxied transparently to the upstream.", &s.proxiedCalls)
	counter("graft_cache_seeded_objects_total", "Objects loaded from seed files.", &s.seededObjects)
	counter("graft_cache_seeded_bytes_total", "Data bytes loaded from seed files.", &s.seededBytes)
	counter("graft_cache_seed_corrupt_total", "Seed records skipped because they were damaged.", &s.seedCorrupt)

	// The hit rates are computed from the counters at scrape time. Each carries the
	// tier as a constant label, so that a scrape of either tier can be told apart.
	rate := func(name, help string, value func(Stats) float64) {
		s.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        name,
			Help:        help,
			ConstLabels: prometheus.Labels{"tier": s.tier},
		}, func() float64 {
			return value(s.Stats())
		}))
	}
	rate("graft_cache_hit_rate", "Share of cache lookups served locally, over objects and trees.", func(st Stats) float64 { return st.HitRate })
	rate("graft_cache_object_hit_rate", "Share of object lookups served from memory or disk.", func(st Stats) float64 { return st.ObjectHitRate })
	rate("graft_cache_tree_hit_rate", "Share of GetTree listings served from memory.", func(st Stats) float64 { return st.TreeHitRate })

	s.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "graft_cache_bytes", Help: "Bytes held by the in-memory LRU.",
	}, func() float64 {
		_, bytes := s.memory.usage()
		return float64(bytes)
	}))
	s.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "graft_cache_entries", Help: "Entries held by the in-memory LRU.",
	}, func() float64 {
		entries, _ := s.memory.usage()
		return float64(entries)
	}))
	s.registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// Stats returns a snapshot of the cache counters.
func (s *Server) Stats() Stats {
	entries, bytes := s.memory.usage()
	objectHits := s.objectHits.Load()
	objectMisses := s.objectMisses.Load()
	diskHits := s.diskHits.Load()
	coalesced := s.coalesced.Load()
	treeHits := s.treeHits.Load()
	treeMisses := s.treeMisses.Load()

	// A disk hit is an object miss of the memory LRU that the disk store served, so
	// it is counted in objectMisses and added back as a hit here.
	objectLookups := objectHits + objectMisses + coalesced
	objectServed := objectHits + diskHits

	return Stats{
		Tier:           s.tier,
		MaxBytes:       s.maxBytes,
		MemoryBytes:    bytes,
		MemoryEntries:  entries,
		ObjectHits:     objectHits,
		ObjectMisses:   objectMisses,
		DiskHits:       diskHits,
		Coalesced:      coalesced,
		TreeHits:       treeHits,
		TreeMisses:     treeMisses,
		Evictions:      s.evictions.Load(),
		VerifyFailures: s.verifyFailures.Load(),
		ProxiedCalls:   s.proxiedCalls.Load(),
		HitRate:        hitRatio(objectServed+treeHits, objectLookups+treeHits+treeMisses),
		ObjectHitRate:  hitRatio(objectServed, objectLookups),
		TreeHitRate:    hitRatio(treeHits, treeHits+treeMisses),
		SeededObjects:  s.seededObjects.Load(),
		SeededBytes:    s.seededBytes.Load(),
		SeedCorrupt:    s.seedCorrupt.Load(),
	}
}

// hitRatio returns hits divided by lookups, or 0 when there were no lookups. It
// never returns NaN, which JSON cannot encode.
func hitRatio(hits, lookups int64) float64 {
	if lookups <= 0 {
		return 0
	}
	return float64(hits) / float64(lookups)
}

// HTTPHandler serves Prometheus metrics on /metrics and the JSON stats on /stats.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.Stats()); err != nil {
			s.logger.WithError(err).Warn("graft-cache: encode stats")
		}
	})
	return mux
}

// GetObjects streams the requested objects in request order, honouring the
// chunking and resume semantics of proto/object.proto.
func (s *Server) GetObjects(req *gitalypb.GetObjectsRequest, stream gitalypb.ObjectService_GetObjectsServer) error {
	ctx := stream.Context()
	repo := req.GetRepository()

	if err := validateRepository(repo); err != nil {
		return err
	}
	if usesAlternateObjects(repo) {
		return s.forwardGetObjects(ctx, req, stream)
	}
	if err := s.authorize(ctx, repo); err != nil {
		return err
	}

	if len(req.GetOids()) == 0 {
		return structerr.NewInvalidArgument("empty object IDs")
	}
	if len(req.GetOids()) > s.maxObjects {
		return structerr.NewInvalidArgument("too many object IDs: %d, maximum is %d", len(req.GetOids()), s.maxObjects)
	}

	oids, err := uniqueOIDs(req.GetOids())
	if err != nil {
		return err
	}

	resumeOID := req.GetResumeOid()
	switch {
	case resumeOID == "" && req.GetResumeOffset() != 0:
		return structerr.NewInvalidArgument("resume_offset requires resume_oid")
	case resumeOID != "" && !containsOID(oids, resumeOID):
		return structerr.NewInvalidArgument("resume_oid is not one of the requested object IDs")
	case req.GetResumeOffset() < 0:
		return structerr.NewInvalidArgument("resume_offset must not be negative")
	}

	// The request holds at most maxObjects IDs, so all of them are loaded with one
	// batched upstream call, or none if everything is already cached.
	objects, err := s.loadObjects(ctx, repo, oids)
	if errors.Is(err, errOverRequestBudget) {
		// Nothing has been sent yet, so the misses can be streamed through the
		// upstream instead. They are not cached, which keeps memory bounded.
		return s.forwardGetObjects(ctx, req, stream)
	}
	if err != nil {
		return err
	}

	for _, oid := range oids {
		obj := objects[oid]
		if obj == nil {
			if err := stream.Send(&gitalypb.GetObjectsResponse{Oid: oid, Missing: true}); err != nil {
				return err
			}
			continue
		}

		var offset int64
		if oid == resumeOID {
			offset = req.GetResumeOffset()
		}
		if err := s.sendObject(stream, oid, obj, offset); err != nil {
			return err
		}
	}

	return nil
}

// sendObject sends obj in chunks of at most chunkSize bytes, starting at offset.
// The first message of the stream carries the object's type and size.
func (s *Server) sendObject(stream gitalypb.ObjectService_GetObjectsServer, oid string, obj *object, offset int64) error {
	size := int64(len(obj.data))
	if offset > size {
		return structerr.NewInvalidArgument("resume_offset %d is beyond the size of object %s (%d bytes)", offset, oid, size)
	}

	for off := offset; ; {
		end := min(off+s.chunkSize, size)

		msg := &gitalypb.GetObjectsResponse{
			Oid:    oid,
			Offset: off,
			Data:   obj.data[off:end],
		}
		if off == offset {
			msg.Type = obj.typ
			msg.Size = size
		}
		if err := stream.Send(msg); err != nil {
			return err
		}

		off = end
		if off >= size {
			return nil
		}
	}
}

// GetTree lists a tree. Listings are cached only when tree_oid selects the
// tree, because a tree ID is immutable. Revision-based requests are forwarded.
func (s *Server) GetTree(req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	ctx := stream.Context()

	repo := req.GetRepository()
	if err := validateRepository(repo); err != nil {
		return err
	}

	if req.GetTreeOid() == "" || usesAlternateObjects(repo) {
		return s.forwardGetTree(ctx, req, stream)
	}

	if !validOID(req.GetTreeOid()) {
		return structerr.NewInvalidArgument("tree_oid is not a valid object ID")
	}
	if len(req.GetRevision()) > 0 {
		return structerr.NewInvalidArgument("tree_oid and revision are mutually exclusive")
	}
	if err := s.authorize(ctx, repo); err != nil {
		return err
	}

	fillBase, err := s.fillContext(ctx)
	if err != nil {
		return err
	}

	key := cacheKey{
		kind: treeKind,
		objectKey: objectKey{
			storage:      repo.GetStorageName(),
			relativePath: repo.GetRelativePath(),
			oid:          req.GetTreeOid(),
		},
		recursive:    req.GetRecursive(),
		includeSizes: req.GetIncludeSizes(),
	}

	if entry, ok := s.memory.get(key); ok {
		s.treeHits.Add(1)
		return s.sendTree(stream, req.GetTreeOid(), entry.entries)
	}
	s.treeMisses.Add(1)

	s.mu.Lock()
	f, waiting := s.treeInflight[key]
	if !waiting {
		f = &treeFlight{done: make(chan struct{})}
		s.treeInflight[key] = f
	}
	s.mu.Unlock()

	if !waiting {
		return s.fillTree(ctx, fillBase, key, f, req, stream)
	}

	select {
	case <-f.done:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return structerr.NewDeadlineExceeded("waiting for tree %s: %v", req.GetTreeOid(), ctx.Err())
		}
		return structerr.NewCanceled("waiting for tree %s: %v", req.GetTreeOid(), ctx.Err())
	}
	if f.cached {
		return s.sendTree(stream, req.GetTreeOid(), f.entries)
	}
	// The listing is too large to cache, or the other request's fill failed,
	// which says nothing about this request. Ask the upstream directly.
	return s.forwardGetTree(ctx, req, stream)
}

// treeFlight is an in-progress fill of one tree listing, shared by the requests
// that need it. done is closed once the fill has either cached the listing, in
// which case cached is set and entries holds it, or given up caching it.
type treeFlight struct {
	done    chan struct{}
	cached  bool
	entries []*gitalypb.GetTreeResponse_Entry
}

// maxTreeBytes bounds the listing that one GetTree request buffers. A listing
// within it fits the LRU and the per-request budget, so it is cached.
func (s *Server) maxTreeBytes() int64 {
	return min(s.maxBytes, s.maxRequestBytes)
}

// fillTree loads a listing for the request that owns the flight f and sends it
// to stream. The listing is buffered while it stays within maxTreeBytes. A
// complete listing within the bound is cached and then sent. Once a listing
// exceeds the bound, it will not be cached: the buffered entries are sent at
// once and the rest is streamed through page by page, so that the tier never
// holds more than maxTreeBytes of one listing. Waiters are released as soon as
// the outcome is known, and f is resolved on every return path.
func (s *Server) fillTree(ctx, fillBase context.Context, key cacheKey, f *treeFlight, req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	resolved := false
	resolve := func(entries []*gitalypb.GetTreeResponse_Entry, cached bool) {
		if resolved {
			return
		}
		resolved = true

		s.mu.Lock()
		if s.treeInflight[key] == f {
			delete(s.treeInflight, key)
		}
		s.mu.Unlock()

		f.entries = entries
		f.cached = cached
		close(f.done)
	}
	defer resolve(nil, false)

	// While the listing may still be cached, the fill serves the waiters too. It
	// is therefore detached from this caller's cancellation, like an object fill,
	// and bounded by fetchTimeout instead.
	fetchCtx, cancel := context.WithCancel(fillBase)
	defer cancel()
	timeout := time.AfterFunc(fetchTimeout, cancel)
	defer timeout.Stop()

	upstream, err := s.upstream.GetTree(fetchCtx, &gitalypb.GetTreeRequest{
		Repository:   req.GetRepository(),
		TreeOid:      req.GetTreeOid(),
		Recursive:    req.GetRecursive(),
		IncludeSizes: req.GetIncludeSizes(),
	})
	if err != nil {
		return err
	}

	var entries []*gitalypb.GetTreeResponse_Entry
	// The first message is always received, so that an upstream error such as an
	// unknown tree reaches the caller before anything has been sent.
	size := int64(entryOverhead)
	for first := true; first || size <= s.maxTreeBytes(); first = false {
		msg, err := upstream.Recv()
		if errors.Is(err, io.EOF) {
			s.memory.add(key, &cacheEntry{entries: entries, size: size})
			resolve(entries, true)
			return s.sendTree(stream, req.GetTreeOid(), entries)
		}
		if err != nil {
			return err
		}

		entries = append(entries, msg.GetEntries()...)
		for _, e := range msg.GetEntries() {
			size += treeEntrySize(e)
		}
	}

	// The listing will not be cached. Release the waiters, which stream it
	// through the upstream themselves, and turn this fill into a plain
	// pass-through that follows this caller's cancellation.
	resolve(nil, false)
	if timeout.Stop() {
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
	}

	// entries is dead after this send, so the buffered listing is released while
	// the rest streams through.
	if err := s.sendTree(stream, req.GetTreeOid(), entries); err != nil {
		return err
	}

	for {
		msg, err := upstream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&gitalypb.GetTreeResponse{Entries: msg.GetEntries()}); err != nil {
			return err
		}
	}
}

// sendTree sends a listing in pages of at most treePageSize entries. The first
// message carries the tree ID, and an empty tree still yields one message.
func (s *Server) sendTree(stream gitalypb.ObjectService_GetTreeServer, treeOID string, entries []*gitalypb.GetTreeResponse_Entry) error {
	if len(entries) == 0 {
		return stream.Send(&gitalypb.GetTreeResponse{TreeOid: treeOID})
	}

	for start := 0; start < len(entries); start += s.treePageSize {
		end := min(start+s.treePageSize, len(entries))
		msg := &gitalypb.GetTreeResponse{Entries: entries[start:end]}
		if start == 0 {
			msg.TreeOid = treeOID
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

// forwardGetTree proxies a GetTree that the cache does not serve, such as a
// revision-based request or one that reads an alternate object directory.
// Nothing is stored.
func (s *Server) forwardGetTree(ctx context.Context, req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	s.proxiedCalls.Add(1)

	outCtx, err := s.outgoingContext(ctx)
	if err != nil {
		return err
	}

	upstreamStream, err := s.upstream.GetTree(outCtx, req)
	if err != nil {
		return err
	}

	for {
		msg, err := upstreamStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
}

// forwardGetObjects proxies a GetObjects request that reads through an alternate
// object directory. Nothing is stored, so the objects of a quarantine never
// reach the cache.
func (s *Server) forwardGetObjects(ctx context.Context, req *gitalypb.GetObjectsRequest, stream gitalypb.ObjectService_GetObjectsServer) error {
	s.proxiedCalls.Add(1)

	outCtx, err := s.outgoingContext(ctx)
	if err != nil {
		return err
	}

	upstreamStream, err := s.upstream.GetObjects(outCtx, req)
	if err != nil {
		return err
	}

	for {
		msg, err := upstreamStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
}

// authorize checks that the caller may read repo. Cached content is served
// without an object read, so the upstream must still confirm the caller's
// credentials on every request. The caller's own authorization is used, or the
// tier's token when the caller sent none. The upstream's error is returned
// unchanged, so a rejected caller gets the same status as a direct call.
func (s *Server) authorize(ctx context.Context, repo *gitalypb.Repository) error {
	outCtx, err := s.outgoingContext(ctx)
	if err != nil {
		return err
	}

	resp, err := s.repos.RepositoryExists(outCtx, &gitalypb.RepositoryExistsRequest{Repository: repo})
	if err != nil {
		return err
	}
	if !resp.GetExists() {
		return structerr.NewNotFound("repository does not exist")
	}
	return nil
}

// usesAlternateObjects reports whether repo reads through a quarantine or
// alternate object directory. Such a view can contain objects that the plain
// repository does not, so its reads must neither be cached nor served from the
// cache.
func usesAlternateObjects(repo *gitalypb.Repository) bool {
	return repo.GetGitObjectDirectory() != "" || len(repo.GetGitAlternateObjectDirectories()) > 0
}

// treeEntrySize approximates the memory one listed entry holds.
func treeEntrySize(e *gitalypb.GetTreeResponse_Entry) int64 {
	return int64(len(e.GetPath()) + len(e.GetOid()) + entryOverhead)
}

func validateRepository(repo *gitalypb.Repository) error {
	if repo.GetStorageName() == "" || repo.GetRelativePath() == "" {
		return structerr.NewInvalidArgument("repository is required with storage_name and relative_path")
	}
	return nil
}

// uniqueOIDs validates oids and drops duplicates, keeping first occurrence order.
func uniqueOIDs(oids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(oids))
	unique := make([]string, 0, len(oids))
	for _, oid := range oids {
		if !validOID(oid) {
			return nil, structerr.NewInvalidArgument("invalid object ID %q", oid)
		}
		if _, ok := seen[oid]; ok {
			continue
		}
		seen[oid] = struct{}{}
		unique = append(unique, oid)
	}
	return unique, nil
}

func containsOID(oids []string, oid string) bool {
	for _, candidate := range oids {
		if candidate == oid {
			return true
		}
	}
	return false
}
