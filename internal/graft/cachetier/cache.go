package cachetier

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	// fetchTimeout bounds one batched upstream fetch. The fetch runs detached
	// from the caller's context so that a client disconnect does not fail the
	// other requests waiting for the same objects.
	fetchTimeout = 5 * time.Minute

	// entryOverhead approximates the per-entry bookkeeping cost in bytes.
	entryOverhead = 128

	// maxPreallocBytes caps the buffer preallocated from an upstream size
	// header, so a bogus size cannot exhaust memory before data arrives.
	maxPreallocBytes = 16 << 20
)

// errOverRequestBudget reports that a request's misses exceed its byte budget.
// The request is then streamed through the upstream instead of being cached.
var errOverRequestBudget = errors.New("the misses of the request exceed its byte budget")

// requestBudget counts the object data that one GetObjects request holds
// outside the in-memory LRU: objects read from disk, objects received from the
// upstream and objects taken over from another request's fill. Objects found in
// the LRU are not counted, because MaxBytes already bounds them. A request is
// served by one goroutine, so the budget needs no locking.
type requestBudget struct {
	max  int64
	used int64
}

// remaining returns the bytes the request may still load.
func (b *requestBudget) remaining() int64 {
	return b.max - b.used
}

// charge counts n more bytes and returns errOverRequestBudget once the request
// holds more than its budget.
func (b *requestBudget) charge(n int64) error {
	b.used += n
	if b.used > b.max {
		return errOverRequestBudget
	}
	return nil
}

// objectKey identifies an object or tree within one repository.
type objectKey struct {
	storage      string
	relativePath string
	oid          string
}

type cacheKind uint8

const (
	objectKind cacheKind = iota + 1
	treeKind
)

// cacheKey identifies a cache entry. Trees are cached per request shape, since
// recursion and sizes change the listing for the same tree ID.
type cacheKey struct {
	kind         cacheKind
	objectKey    objectKey
	recursive    bool
	includeSizes bool
}

// cacheEntry is either a single object or a tree listing.
type cacheEntry struct {
	obj     *object
	entries []*gitalypb.GetTreeResponse_Entry
	size    int64
}

// flight is an in-progress load of one object shared by all requests that
// need it. done is closed once obj or err has been set.
type flight struct {
	done chan struct{}
	obj  *object
	err  error
}

// ownedFetch is an object this request is responsible for loading.
type ownedFetch struct {
	key    objectKey
	flight *flight
}

// waitingFetch is an object another request is already loading.
type waitingFetch struct {
	oid    string
	flight *flight
}

// loadObjects returns the objects for oids keyed by object ID. A missing object
// maps to nil. Objects are served from memory, then disk, then one batched
// upstream fetch. Concurrent loads of the same object are coalesced.
//
// Every object that is not served from the LRU counts against the request's
// budget of maxRequestBytes. A request that would exceed it fails with
// errOverRequestBudget before it holds more than the budget, so that the caller
// can stream it through the upstream instead.
func (s *Server) loadObjects(ctx context.Context, repo *gitalypb.Repository, oids []string) (map[string]*object, error) {
	result := make(map[string]*object, len(oids))
	budget := &requestBudget{max: s.maxRequestBytes}

	pending := oids
	for len(pending) > 0 {
		retry, err := s.loadPass(ctx, repo, pending, result, budget)
		if err != nil {
			return nil, err
		}
		pending = retry
	}

	return result, nil
}

// loadPass makes one attempt to load oids into result. It returns the object IDs
// whose coalesced load failed in another request. Those must be retried, because
// a failure of another client's fill says nothing about this request. The retry
// either finds the object in memory, owns a new fill with this request's own
// credentials, or joins a newer flight.
func (s *Server) loadPass(ctx context.Context, repo *gitalypb.Repository, oids []string, result map[string]*object, budget *requestBudget) ([]string, error) {
	var owned []ownedFetch
	var waiting []waitingFetch

	// Classification happens under one lock so that a request either owns a
	// flight or waits on one, never both. This rules out waits cycling between
	// two requests that each own what the other waits for.
	s.mu.Lock()
	for _, oid := range oids {
		key := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oid}

		if entry, ok := s.memory.get(cacheKey{kind: objectKind, objectKey: key}); ok {
			s.objectHits.Add(1)
			result[oid] = entry.obj
			continue
		}

		if f, ok := s.inflight[key]; ok {
			s.coalesced.Add(1)
			waiting = append(waiting, waitingFetch{oid: oid, flight: f})
			continue
		}

		s.objectMisses.Add(1)
		f := &flight{done: make(chan struct{})}
		s.inflight[key] = f
		owned = append(owned, ownedFetch{key: key, flight: f})
	}
	s.mu.Unlock()

	if len(owned) > 0 {
		loaded, err := s.fetchOwned(ctx, repo, owned, budget)
		if err != nil {
			return nil, err
		}
		for oid, obj := range loaded {
			result[oid] = obj
		}
	}

	var retry []string
	for _, w := range waiting {
		select {
		case <-w.flight.done:
		case <-ctx.Done():
			// The caller gave up, so report its own cancellation rather than
			// Unavailable. Clients that retry Unavailable would otherwise retry a
			// request the caller already abandoned.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, structerr.NewDeadlineExceeded("waiting for object %s: %v", w.oid, ctx.Err())
			}
			return nil, structerr.NewCanceled("waiting for object %s: %v", w.oid, ctx.Err())
		}
		if w.flight.err != nil {
			retry = append(retry, w.oid)
			continue
		}
		// The owner's request may finish and the LRU may drop the object, while
		// this request still holds it. It therefore counts against this budget too.
		if obj := w.flight.obj; obj != nil {
			if err := budget.charge(int64(len(obj.data))); err != nil {
				return nil, err
			}
		}
		result[w.oid] = w.flight.obj
	}

	return retry, nil
}

// fillContext returns the context for an upstream fill that serves the request
// carrying ctx. The fill is detached from the request's cancellation, so that
// one client's disconnect does not fail the other requests waiting for the same
// objects. It keeps the request's outgoing metadata, which is the only way its
// credentials reach the upstream.
func (s *Server) fillContext(ctx context.Context) (context.Context, error) {
	outCtx, err := s.outgoingContext(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithoutCancel(outCtx), nil
}

// fetchOwned loads the objects this request owns and resolves their flights.
// Every flight is resolved before fetchOwned returns, on success and on error.
// Disk hits and upstream misses both count against budget.
func (s *Server) fetchOwned(ctx context.Context, repo *gitalypb.Repository, owned []ownedFetch, budget *requestBudget) (map[string]*object, error) {
	loaded := make(map[string]*object, len(owned))
	var remaining []ownedFetch

	for i, o := range owned {
		if s.disk != nil {
			// The size is checked against the budget before the object is
			// allocated, so a request cannot buffer more than its budget of
			// objects that only the disk store holds.
			obj, err := s.disk.read(o.key, budget.remaining())
			if obj != nil {
				err = budget.charge(int64(len(obj.data)))
			}
			if errors.Is(err, errObjectOverLimit) || errors.Is(err, errOverRequestBudget) {
				// Release the flights that were not loaded, so that their waiters
				// retry with their own budgets.
				for _, rest := range append(remaining, owned[i:]...) {
					s.resolve(rest, nil, errOverRequestBudget)
				}
				return nil, errOverRequestBudget
			}
			if err != nil {
				s.logger.WithError(err).WithField("oid", o.key.oid).Warn("graft-cache: read object from disk")
			}
			if obj != nil {
				s.diskHits.Add(1)
				s.store(o.key, obj, false)
				loaded[o.key.oid] = obj
				s.resolve(o, obj, nil)
				continue
			}
		}
		remaining = append(remaining, o)
	}

	if len(remaining) == 0 {
		return loaded, nil
	}

	oids := make([]string, 0, len(remaining))
	for _, o := range remaining {
		oids = append(oids, o.key.oid)
	}

	base, err := s.fillContext(ctx)
	if err != nil {
		for _, o := range remaining {
			s.resolve(o, nil, err)
		}
		return nil, err
	}

	fetchCtx, cancel := context.WithTimeout(base, fetchTimeout)
	defer cancel()

	fetched, err := s.fetchFromUpstream(fetchCtx, repo, oids, budget)
	if err != nil {
		for _, o := range remaining {
			s.resolve(o, nil, err)
		}
		return nil, err
	}

	for _, o := range remaining {
		obj := fetched[o.key.oid]
		if obj != nil {
			s.store(o.key, obj, true)
			loaded[o.key.oid] = obj
		}
		s.resolve(o, obj, nil)
	}

	return loaded, nil
}

// resolve publishes the outcome of an owned fetch and wakes all waiters.
func (s *Server) resolve(o ownedFetch, obj *object, err error) {
	s.mu.Lock()
	if s.inflight[o.key] == o.flight {
		delete(s.inflight, o.key)
	}
	s.mu.Unlock()

	o.flight.obj = obj
	o.flight.err = err
	close(o.flight.done)
}

// store puts a verified object into memory and, if requested and configured,
// onto disk. Disk failures are logged and never fail the request.
func (s *Server) store(key objectKey, obj *object, toDisk bool) {
	// The entry keeps the whole backing array alive, so its capacity is what counts
	// against the memory budget.
	s.memory.add(cacheKey{kind: objectKind, objectKey: key}, &cacheEntry{obj: obj, size: int64(cap(obj.data)) + entryOverhead})

	if toDisk && s.disk != nil {
		if err := s.disk.write(key, obj); err != nil {
			s.logger.WithError(err).WithField("oid", key.oid).Warn("graft-cache: write object to disk")
		}
	}
}

// fetchFromUpstream loads oids with one GetObjects call and verifies every
// object it receives. Missing objects are returned as nil entries. The data it
// receives counts against budget.
func (s *Server) fetchFromUpstream(ctx context.Context, repo *gitalypb.Repository, oids []string, budget *requestBudget) (map[string]*object, error) {
	stream, err := s.upstream.GetObjects(ctx, &gitalypb.GetObjectsRequest{
		Repository: repo,
		Oids:       oids,
	})
	if err != nil {
		return nil, err
	}

	requested := make(map[string]bool, len(oids))
	for _, oid := range oids {
		requested[oid] = true
	}

	result := make(map[string]*object, len(oids))

	var cur *partialObject
	finish := func() error {
		if cur == nil {
			return nil
		}
		p := cur
		cur = nil

		if int64(len(p.obj.data)) != p.size {
			return structerr.NewDataLoss("upstream object %s: got %d of %d bytes", p.oid, len(p.obj.data), p.size)
		}
		if err := verifyObject(p.oid, p.obj); err != nil {
			s.verifyFailures.Add(1)
			return structerr.NewDataLoss("upstream object %s: %v", p.oid, err)
		}
		result[p.oid] = p.obj
		return nil
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		if cur != nil && msg.GetOid() != cur.oid {
			if err := finish(); err != nil {
				return nil, err
			}
		}

		oid := msg.GetOid()
		if !requested[oid] {
			return nil, structerr.NewInternal("upstream returned unrequested object %q", oid)
		}
		if _, seen := result[oid]; seen {
			return nil, structerr.NewInternal("upstream returned object %s twice", oid)
		}

		if msg.GetMissing() {
			result[oid] = nil
			continue
		}

		if cur == nil {
			if msg.GetOffset() != 0 {
				return nil, structerr.NewInternal("upstream object %s starts at offset %d", oid, msg.GetOffset())
			}
			if msg.GetSize() < 0 {
				return nil, structerr.NewInternal("upstream object %s has negative size", oid)
			}
			if _, ok := typeName(msg.GetType()); !ok {
				return nil, structerr.NewInternal("upstream object %s has unsupported type %v", oid, msg.GetType())
			}
			prealloc := msg.GetSize()
			if prealloc > maxPreallocBytes {
				prealloc = maxPreallocBytes
			}
			cur = &partialObject{
				oid:  oid,
				size: msg.GetSize(),
				obj:  &object{typ: msg.GetType(), data: make([]byte, 0, prealloc)},
			}
		}

		if msg.GetOffset() != int64(len(cur.obj.data)) {
			return nil, structerr.NewInternal("upstream object %s: chunk at offset %d, expected %d", oid, msg.GetOffset(), len(cur.obj.data))
		}
		if int64(len(cur.obj.data))+int64(len(msg.GetData())) > cur.size {
			return nil, structerr.NewInternal("upstream object %s: more data than its declared size", oid)
		}
		// The budget spans all objects of the request, including those read from
		// disk, so that the request's memory stays within maxRequestBytes.
		if err := budget.charge(int64(len(msg.GetData()))); err != nil {
			return nil, err
		}
		cur.obj.data = appendBounded(cur.obj.data, msg.GetData(), cur.size)
	}

	if err := finish(); err != nil {
		return nil, err
	}

	for _, oid := range oids {
		if _, ok := result[oid]; !ok {
			return nil, structerr.NewInternal("upstream did not return object %s", oid)
		}
	}

	return result, nil
}

// appendBounded appends chunk to data like append, but never grows the capacity
// beyond limit, the declared size of the object. append's growth policy would leave
// the backing array of a large object up to ~25% larger than its content, memory
// that the cache keeps alive without accounting for it. A complete object, whose
// length equals its declared size, therefore has no spare capacity. The caller
// guarantees that len(data)+len(chunk) <= limit.
func appendBounded(data, chunk []byte, limit int64) []byte {
	need := len(data) + len(chunk)
	if need <= cap(data) {
		return append(data, chunk...)
	}

	newCap := int64(max(2*cap(data), need))
	if newCap > limit {
		newCap = limit
	}

	grown := make([]byte, len(data), newCap)
	copy(grown, data)
	return append(grown, chunk...)
}

// partialObject is an object whose chunks are still being received.
type partialObject struct {
	oid  string
	size int64
	obj  *object
}
