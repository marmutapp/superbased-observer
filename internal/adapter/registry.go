package adapter

import (
	"os"
	"sort"
	"sync"
)

// Registry holds all known adapters, keyed by Adapter.Name().
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register adds an adapter. Later registrations with the same Name overwrite
// earlier ones — intended for testing, not for runtime hot-swapping.
func (r *Registry) Register(a Adapter) {
	if a == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

// Get returns the adapter registered under name, or nil if none.
func (r *Registry) Get(name string) Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adapters[name]
}

// All returns every registered adapter in Name-sorted order.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Adapter, 0, len(names))
	for _, n := range names {
		out = append(out, r.adapters[n])
	}
	return out
}

// Detected filters the registry down to adapters whose WatchPaths include
// at least one directory that currently exists. The allow list semantics
// distinguish nil from empty:
//
//   - allow == nil           → no filter (all adapters considered).
//     This is the zero-value case for callers that don't pass a
//     restriction (e.g. unit tests using watcher.Options{}).
//   - len(allow) == 0, !nil  → filter to *zero* adapters. This is the
//     explicit user-intent case: a TOML
//     `enabled_adapters = []` should disable the watcher, not silently
//     fall through to "all". BurntSushi/toml preserves the nil vs.
//     empty-slice distinction (key absent → nil-or-default, key
//     present with `[]` → non-nil empty slice).
//   - len(allow) > 0          → restrict to the named adapters.
func (r *Registry) Detected(allow []string) []Adapter {
	if allow != nil && len(allow) == 0 {
		return nil
	}
	allowSet := map[string]struct{}{}
	for _, n := range allow {
		allowSet[n] = struct{}{}
	}
	var out []Adapter
	for _, a := range r.All() {
		if len(allowSet) > 0 {
			if _, ok := allowSet[a.Name()]; !ok {
				continue
			}
		}
		if anyDirExists(a.WatchPaths()) {
			out = append(out, a)
		}
	}
	return out
}

func anyDirExists(paths []string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}
