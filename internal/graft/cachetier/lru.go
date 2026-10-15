package cachetier

import (
	"container/list"
	"sync"
)

// lru is a least-recently-used cache bounded by the total size of its values
// as reported by sizeOf. It is safe for concurrent use.
type lru[K comparable, V any] struct {
	mu        sync.Mutex
	maxBytes  int64
	bytes     int64
	items     map[K]*list.Element
	order     *list.List
	sizeOf    func(V) int64
	onEvicted func()
}

type lruEntry[K comparable, V any] struct {
	key  K
	val  V
	size int64
}

// newLRU returns an LRU that holds at most maxBytes worth of values. onEvicted,
// if non-nil, is invoked once for every entry removed to make room.
func newLRU[K comparable, V any](maxBytes int64, sizeOf func(V) int64, onEvicted func()) *lru[K, V] {
	return &lru[K, V]{
		maxBytes:  maxBytes,
		items:     make(map[K]*list.Element),
		order:     list.New(),
		sizeOf:    sizeOf,
		onEvicted: onEvicted,
	}
}

// get returns the value for key and marks it as most recently used.
func (c *lru[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*lruEntry[K, V]).val, true
}

// add stores val under key. Values larger than the whole budget are not stored.
// Least recently used entries are evicted until the budget is respected again.
func (c *lru[K, V]) add(key K, val V) {
	size := c.sizeOf(val)
	if size > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.removeElement(elem)
	}

	elem := c.order.PushFront(&lruEntry[K, V]{key: key, val: val, size: size})
	c.items[key] = elem
	c.bytes += size

	for c.bytes > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeElement(oldest)
		if c.onEvicted != nil {
			c.onEvicted()
		}
	}
}

// removeElement must be called with c.mu held.
func (c *lru[K, V]) removeElement(elem *list.Element) {
	entry := c.order.Remove(elem).(*lruEntry[K, V])
	delete(c.items, entry.key)
	c.bytes -= entry.size
}

// usage returns the number of entries and the number of bytes currently held.
func (c *lru[K, V]) usage() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.bytes
}
