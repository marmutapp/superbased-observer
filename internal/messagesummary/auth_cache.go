package messagesummary

import (
	"sync"
)

// AuthCredentials carries the headers the [AnthropicSummarizer] needs
// to authenticate one /v1/messages call. Either Authorization (Pro/Max
// OAuth bearer) or APIKey is populated — the production proxy
// captures whichever was on the wire. Both empty means we have no
// auth to use; the summariser bails on that path rather than sending
// an unauthenticated request.
type AuthCredentials struct {
	Authorization string
	APIKey        string
}

// Empty reports whether neither credential field is populated.
func (a AuthCredentials) Empty() bool {
	return a.Authorization == "" && a.APIKey == ""
}

// AuthCache is the process-wide session_id → credentials map. The
// proxy's serve() writes one entry per Anthropic request (per session)
// before invoking the compressor; the rolling summariser reads it
// when threshold fires.
//
// Soft cap: when entries exceed Capacity, the cache resets — same
// drop-everything strategy as [conversation.summaryCache] (a session-
// scoped LRU was considered, but real workloads don't hit the cap on
// any sane Capacity).
type AuthCache struct {
	mu       sync.RWMutex
	creds    map[string]AuthCredentials
	capacity int
}

// NewAuthCache constructs an AuthCache. Capacity ≤ 0 defaults to 1024
// — large enough that even multi-tenant proxies don't churn it.
func NewAuthCache(capacity int) *AuthCache {
	if capacity <= 0 {
		capacity = 1024
	}
	return &AuthCache{
		creds:    map[string]AuthCredentials{},
		capacity: capacity,
	}
}

// Set replaces the credentials for sessionID. Empty sessionID is a
// no-op so a missed extraction can't corrupt the cache.
func (c *AuthCache) Set(sessionID string, creds AuthCredentials) {
	if sessionID == "" || creds.Empty() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.creds) >= c.capacity {
		c.creds = map[string]AuthCredentials{}
	}
	c.creds[sessionID] = creds
}

// Get returns the credentials for sessionID, or zero-value when
// absent.
func (c *AuthCache) Get(sessionID string) (AuthCredentials, bool) {
	if sessionID == "" {
		return AuthCredentials{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	creds, ok := c.creds[sessionID]
	return creds, ok
}

// Len returns the current cache size (test-only — production code
// shouldn't depend on this for correctness).
func (c *AuthCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.creds)
}
