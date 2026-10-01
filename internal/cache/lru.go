package cache

import (
	"container/list"
	"sync"
	"time"
)

// EvictReason distinguishes why an entry left the cache, passed to the
// OnEvict hook so a later metrics phase can label evictions correctly
// (capacity pressure is a capacity-planning signal; nothing else evicts
// entries today, but the reason type leaves room for e.g. a manual purge
// later).
type EvictReason string

const (
	EvictReasonCapacity EvictReason = "capacity"
)

// Hooks lets callers observe cache activity without this package
// depending on a metrics or logging package. A later observability phase
// plugs Prometheus counters/gauges in here instead of modifying this file.
type Hooks struct {
	OnEvict func(key string, reason EvictReason)
}

type lruNode struct {
	key   string
	entry Entry
}

// LRU is an in-memory, fixed-capacity cache with least-recently-used
// eviction. It is safe for concurrent use. See the Cache interface
// doc-comment for the in-memory-vs-Redis tradeoff this implementation
// represents.
type LRU struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element // key -> node in order
	order    *list.List               // front = most recently used, back = least
	hooks    Hooks
}

// NewLRU creates an LRU cache that holds at most capacity entries.
// capacity must be >= 1.
func NewLRU(capacity int, hooks Hooks) *LRU {
	if capacity < 1 {
		capacity = 1
	}
	return &LRU{
		capacity: capacity,
		items:    make(map[string]*list.Element, capacity),
		order:    list.New(),
		hooks:    hooks,
	}
}

// Get returns the entry for key, marking it most-recently-used on a hit.
func (c *LRU) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return Entry{}, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*lruNode).entry, true
}

// Set stores entry for key, evicting the least-recently-used entry if
// the cache is at capacity and key is new.
func (c *LRU) Set(key string, entry Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		elem.Value.(*lruNode).entry = entry
		c.order.MoveToFront(elem)
		return
	}

	if c.order.Len() >= c.capacity {
		c.evictOldestLocked()
	}

	elem := c.order.PushFront(&lruNode{key: key, entry: entry})
	c.items[key] = elem
}

// evictOldestLocked removes the least-recently-used entry. Caller must
// hold c.mu.
func (c *LRU) evictOldestLocked() {
	oldest := c.order.Back()
	if oldest == nil {
		return
	}
	node := oldest.Value.(*lruNode)
	c.order.Remove(oldest)
	delete(c.items, node.key)
	if c.hooks.OnEvict != nil {
		c.hooks.OnEvict(node.key, EvictReasonCapacity)
	}
}

// Len reports the current number of entries.
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// OldestAge reports the age of the entry with the oldest FetchedAt
// currently in the cache, or zero if empty. This is a straight scan
// rather than tracked incrementally: cache sizes here are small (at most
// a few thousand currency-pair combinations) and this is only called by
// the infrequent /ready probe, so the simplicity is worth more than the
// O(n) cost. Note this is independent of LRU recency-of-access order —
// the least-recently-*used* entry is not necessarily the oldest by fetch
// time, which is what /ready actually wants to report.
func (c *LRU) OldestAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	var oldest time.Duration
	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		age := elem.Value.(*lruNode).entry.Age()
		if age > oldest {
			oldest = age
		}
	}
	return oldest
}
