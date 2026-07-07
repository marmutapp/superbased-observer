package rollup

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-rollup-type default TTLs (build prompt §M3: overview 60s; team / project
// / developer 30s; audit 5s).
const (
	TTLOverview = 60 * time.Second
	TTLTeam     = 30 * time.Second
	TTLProject  = 30 * time.Second
	TTLDevs     = 30 * time.Second
	TTLBudgets  = 30 * time.Second
	TTLAudit    = 5 * time.Second
	// TTLGuard covers the §14.3 guard rollups (overview / rules / teams).
	// The audited per-agent chain report is deliberately uncached — every
	// disclosure must hit its audit write.
	TTLGuard = 30 * time.Second
)

type cacheEntry struct {
	value   any
	expires time.Time
}

// Cache is a tiny read-through TTL cache for rollup results. It is purely an
// accelerator — it holds no authority, so any entry may be dropped at any time
// (a budget write, say, calls Invalidate). Concurrency-safe. Keys are opaque
// strings composed by the caller from the rollup type, scope, and window. The
// keyspace is bounded (a few types × windows × scopes), so eviction is a simple
// expired-sweep with a wholesale clear as the backstop at the cap.
type Cache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	maxEntries int
	now        func() time.Time
}

// NewCache returns a Cache holding up to maxEntries (default 512) entries.
func NewCache(maxEntries int) *Cache {
	if maxEntries <= 0 {
		maxEntries = 512
	}
	return &Cache{entries: map[string]cacheEntry{}, maxEntries: maxEntries, now: time.Now}
}

func (c *Cache) load(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	// Load outside the lock; a concurrent duplicate load is acceptable (no
	// singleflight — "nothing fancy"). Errors are never cached.
	v, err := fn()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.entries) >= c.maxEntries {
		c.sweep(now)
	}
	c.entries[key] = cacheEntry{value: v, expires: now.Add(ttl)}
	c.mu.Unlock()
	return v, nil
}

// sweep removes expired entries; if still at cap it clears the map wholesale.
// Caller holds mu.
func (c *Cache) sweep(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= c.maxEntries {
		c.entries = map[string]cacheEntry{}
	}
}

// Invalidate drops all cached entries (call after a mutation that changes the
// underlying data, e.g. a budget create/update/delete).
func (c *Cache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = map[string]cacheEntry{}
	c.mu.Unlock()
}

// Cached is the generic read-through wrapper the handlers use. A nil Cache
// degrades to a direct load (uncached), which keeps tests and the
// cache-disabled path trivial.
func Cached[T any](c *Cache, key string, ttl time.Duration, load func() (T, error)) (T, error) {
	if c == nil {
		return load()
	}
	v, err := c.load(key, ttl, func() (any, error) { return load() })
	if err != nil {
		var zero T
		return zero, err
	}
	return v.(T), nil
}

// CacheKey composes a cache key from a rollup type tag, the caller's scope, the
// window, and any extra discriminators (a team/project id, audit limit/offset).
func CacheKey(kind string, scope Scope, w Window, extra ...string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte('|')
	b.WriteString(scope.cacheKey())
	b.WriteByte('|')
	b.WriteString("d")
	b.WriteString(strconv.Itoa(w.days()))
	for _, e := range extra {
		b.WriteByte('|')
		b.WriteString(e)
	}
	return b.String()
}

// cacheKey renders a scope into a stable key fragment ("admin" or the sorted
// led-team id set) PLUS any active global filters, so two requests with the
// same authority but different tool/project filters never share a cache slot.
func (s Scope) cacheKey() string {
	var auth string
	if s.Admin {
		auth = "admin"
	} else {
		ids := append([]string(nil), s.TeamIDs...)
		sort.Strings(ids)
		auth = "lead:" + strings.Join(ids, ",")
	}
	if s.Filters.Tool == "" {
		return auth
	}
	return auth + ";tool:" + s.Filters.Tool
}
