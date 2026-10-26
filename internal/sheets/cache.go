package sheets

import (
	"container/list"
	"context"
	"sync"
	"time"

	"ludworks/internal/sheets/engine"
)

// Cache limits. A single workbook may hold up to engine.MaxCellsPerWorkbook cells; the cache
// keeps several of them loaded and evicts the least recently used idle ones beyond the
// bounds (entries in use are never evicted).
const (
	defaultCacheCells   = 2_000_000
	defaultCacheEntries = 64
	defaultIdleTimeout  = 15 * time.Minute
)

// entry is one cached workbook. Its engine and meta are guarded by the per-workbook lock
// (sem); bookkeeping fields are guarded by the cache mutex.
type entry struct {
	id  string
	sem chan struct{} // per-workbook mutex that honours request contexts

	// guarded by sem
	eng  *engine.Engine // nil until loaded (or after invalidation)
	meta wbMeta

	// guarded by cache.mu
	owner    string // set once loaded (immutable for a workbook)
	cells    int
	loaded   bool
	refs     int
	lastUsed time.Time
	elem     *list.Element
}

// cache is an LRU of loaded workbook engines bounded by total cells and entry count, with
// idle eviction.
type cache struct {
	mu         sync.Mutex
	m          map[string]*entry
	lru        *list.List // front = most recently used
	total      int
	maxCells   int
	maxEntries int
	idle       time.Duration
	now        func() time.Time
}

func newCache(maxCells, maxEntries int, idle time.Duration) *cache {
	return &cache{m: map[string]*entry{}, lru: list.New(), maxCells: maxCells, maxEntries: maxEntries,
		idle: idle, now: time.Now}
}

// acquire returns the entry for id with its lock held. When the cached owner differs from
// owner it fails fast with found=false (callers answer 404). ctx bounds the wait.
func (c *cache) acquire(ctx context.Context, id, owner string) (e *entry, found bool, err error) {
	c.mu.Lock()
	e = c.m[id]
	if e != nil && e.loaded && e.owner != owner {
		c.mu.Unlock()
		return nil, false, nil
	}
	if e == nil {
		e = &entry{id: id, sem: make(chan struct{}, 1)}
		c.m[id] = e
		e.elem = c.lru.PushFront(e)
	} else {
		c.lru.MoveToFront(e.elem)
	}
	e.refs++
	c.mu.Unlock()
	select {
	case e.sem <- struct{}{}:
		return e, true, nil
	case <-ctx.Done():
		c.mu.Lock()
		c.unrefLocked(e)
		c.mu.Unlock()
		return nil, false, ctx.Err()
	}
}

// release records the entry's state and unlocks it. A broken or unloaded engine is dropped
// so the next request reloads the workbook from the database.
func (c *cache) release(e *entry) {
	if e.eng != nil && e.eng.Broken() {
		e.eng = nil
	}
	loaded := e.eng != nil
	cells := 0
	if loaded {
		cells = e.eng.CellCount()
	}
	owner := e.meta.OwnerID
	c.mu.Lock()
	if c.m[e.id] == e {
		c.total += cells - e.cells
	}
	e.cells, e.loaded = cells, loaded
	if loaded {
		e.owner = owner
	}
	e.lastUsed = c.now()
	c.unrefLocked(e)
	c.evictLocked(false)
	c.mu.Unlock()
	<-e.sem
}

// unrefLocked drops one reference; unloaded entries nobody waits for are removed.
func (c *cache) unrefLocked(e *entry) {
	e.refs--
	if e.refs == 0 && !e.loaded {
		c.removeLocked(e)
	}
}

func (c *cache) removeLocked(e *entry) {
	if c.m[e.id] != e {
		return
	}
	delete(c.m, e.id)
	c.lru.Remove(e.elem)
	c.total -= e.cells
	e.cells, e.loaded = 0, false
	e.eng = nil // safe: refs == 0, so nobody holds or waits for the lock
}

// evictLocked removes least recently used idle entries beyond the limits (and, when
// idleOnly, every entry unused for longer than the idle timeout).
func (c *cache) evictLocked(idleOnly bool) {
	now := c.now()
	for el := c.lru.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*entry)
		if e.refs == 0 {
			over := c.total > c.maxCells || len(c.m) > c.maxEntries
			stale := c.idle > 0 && now.Sub(e.lastUsed) > c.idle
			if (!idleOnly && over) || stale {
				c.removeLocked(e)
			}
		}
		el = prev
	}
}

// evictIdle drops entries that have not been used for the idle timeout.
func (c *cache) evictIdle() {
	c.mu.Lock()
	c.evictLocked(true)
	c.mu.Unlock()
}

// forget removes an entry (e.g. after the workbook was deleted); the caller holds its lock.
func (c *cache) forget(e *entry) {
	e.eng = nil
}

// dropOwner removes every idle entry of a deleted user. Entries in use are dropped when
// released (their engine is cleared by the deleting request or fails its next commit).
func (c *cache) dropOwner(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.m {
		if e.owner == owner && e.refs == 0 {
			c.removeLocked(e)
		}
	}
}

// stats reports the number of cached entries and total cells (tests, diagnostics).
func (c *cache) stats() (entries, cells int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m), c.total
}
