package proxy

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/cachetrack"
)

// TestProxy_CacheEngine_Getter guards the seam the daemon relies on to
// share one cachetrack engine between the proxy (Tier-1) and the watcher
// (Tier-2): `observer start` reads Proxy.CacheEngine() and hands it to
// Watcher.SetCacheEngine. If this getter ever stops returning the wired
// instance, non-proxied sessions silently lose all cache_entries.
func TestProxy_CacheEngine_Getter(t *testing.T) {
	t.Parallel()

	t.Run("returns the wired engine", func(t *testing.T) {
		eng := cachetrack.NewEngine(8)
		p, err := New(Options{
			AnthropicUpstream: "http://127.0.0.1:0",
			OpenAIUpstream:    "http://127.0.0.1:0",
			Sink:              &fakeSink{},
			CacheEngine:       eng,
		})
		if err != nil {
			t.Fatalf("proxy.New: %v", err)
		}
		if p.CacheEngine() != eng {
			t.Errorf("CacheEngine() = %p, want the wired instance %p", p.CacheEngine(), eng)
		}
	})

	t.Run("nil when cache tracking disabled", func(t *testing.T) {
		p, err := New(Options{
			AnthropicUpstream: "http://127.0.0.1:0",
			OpenAIUpstream:    "http://127.0.0.1:0",
			Sink:              &fakeSink{},
		})
		if err != nil {
			t.Fatalf("proxy.New: %v", err)
		}
		if p.CacheEngine() != nil {
			t.Errorf("CacheEngine() = %p, want nil (no engine wired)", p.CacheEngine())
		}
	})
}
