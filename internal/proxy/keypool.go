package proxy

import (
	"net/http"
	"strings"
	"sync/atomic"
)

// Multi-key rotation (§R12.4): a same-provider key pool rotated on
// rate-limit pressure — LiteLLM key-pool parity for the single node.
// The pool lives ONLY in the node's local config.toml ([routing.
// key_pool]); it is never synced, never pushed, and key material is
// never logged (rotation logs an index, nothing else). The §R11.5
// keychain-backed vault is the P3 upgrade path.
//
// Rotation guard (G7): keys swap ONLY into requests that already
// carried the same credential form — an x-api-key header rotates
// x-api-key; a Bearer sk-* rotates the bearer. OAuth / JWT requests
// (subscription entitlement) are NEVER touched: substituting an API
// key under a subscription request changes billing semantics.

// keyPool is one provider's rotating key ring.
type keyPool struct {
	keys []string
	next atomic.Uint64
}

// buildKeyPools converts the Options map.
func buildKeyPools(cfg map[string][]string) map[string]*keyPool {
	if len(cfg) == 0 {
		return nil
	}
	out := make(map[string]*keyPool, len(cfg))
	for provider, keys := range cfg {
		clean := make([]string, 0, len(keys))
		for _, k := range keys {
			if k != "" {
				clean = append(clean, k)
			}
		}
		if len(clean) > 0 {
			out[provider] = &keyPool{keys: clean}
		}
	}
	return out
}

// rotateAuth applies the pool's next key to the request, honoring the
// credential-form guard. Returns false (request untouched) when the
// request's auth form is not rotatable or no pool exists.
func (p *Proxy) rotateAuth(req *http.Request, provider string) bool {
	pool, ok := p.keyPools[provider]
	if !ok || len(pool.keys) < 2 {
		return false
	}
	key := pool.keys[pool.next.Add(1)%uint64(len(pool.keys))]
	switch {
	case req.Header.Get("x-api-key") != "":
		req.Header.Set("x-api-key", key)
		return true
	case strings.HasPrefix(req.Header.Get("Authorization"), "Bearer sk-") &&
		!strings.HasPrefix(req.Header.Get("Authorization"), "Bearer sk-ant-oat"):
		req.Header.Set("Authorization", "Bearer "+key)
		return true
	default:
		// OAuth / JWT / unauthenticated: never substitute (G7).
		return false
	}
}
