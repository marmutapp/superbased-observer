package profilerouter

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fake is the test compressor: identity matters (pointer equality
// proves instance reuse), the name records which profile built it.
type fake struct{ profile string }

func testRouter(t *testing.T, assignments map[string]string, opts func(*Options[*fake])) (*Router[*fake], *int) {
	t.Helper()
	builds := 0
	o := Options[*fake]{
		Resolve: func(provider, tool, cwd, _ string) string {
			if n, ok := assignments["cwd:"+cwd]; ok && cwd != "" {
				return n
			}
			if n, ok := assignments["tool:"+tool]; ok && tool != "" {
				return n
			}
			if n, ok := assignments[provider]; ok {
				return n
			}
			return "default"
		},
		Build: func(name string) (*fake, error) {
			builds++
			return &fake{profile: name}, nil
		},
		DefaultProfile: "default",
		Fallback:       &fake{profile: "default"},
	}
	if opts != nil {
		opts(&o)
	}
	return New(o), &builds
}

// TestFor_ResolvesPerProviderAndCachesInstances is the Track-R core:
// two providers resolve to their own profiles, each built exactly
// once, with every later session reusing the cached instance.
func TestFor_ResolvesPerProviderAndCachesInstances(t *testing.T) {
	r, builds := testRouter(t, map[string]string{"anthropic": "claude-code", "openai": "codex-safe"}, nil)

	a1 := r.For("anthropic", "", "", "sess-a1")
	o1 := r.For("openai", "", "", "sess-o1")
	a2 := r.For("anthropic", "", "", "sess-a2")

	if a1.profile != "claude-code" || o1.profile != "codex-safe" {
		t.Fatalf("resolution: anthropic→%q openai→%q", a1.profile, o1.profile)
	}
	if a1 != a2 {
		t.Error("same profile, different sessions: instance must be shared")
	}
	if a1 == o1 {
		t.Error("different profiles must get different instances")
	}
	if *builds != 2 {
		t.Errorf("Build called %d times, want 2 (once per profile)", *builds)
	}
}

// TestFor_SessionSticky pins the session contract: a session keeps
// the instance it first resolved even after Update bumps the
// assignment version, while new sessions resolve fresh.
func TestFor_SessionSticky(t *testing.T) {
	r, _ := testRouter(t, map[string]string{"anthropic": "claude-code"}, nil)

	before := r.For("anthropic", "", "", "sess-old")
	r.Update(func(provider, tool, cwd, _ string) string { return "codex-variant" }, &fake{profile: "default"})

	if got := r.For("anthropic", "", "", "sess-old"); got != before {
		t.Error("existing session lost its instance across Update")
	}
	after := r.For("anthropic", "", "", "sess-new")
	if after.profile != "codex-variant" {
		t.Errorf("new session post-Update resolved %q, want codex-variant", after.profile)
	}
	if after == before {
		t.Error("post-Update build must be a fresh instance")
	}
}

// TestUpdate_SwapsFallback pins the master-params half of hot reload:
// Update re-seeds the default profile from the NEW fallback (master
// parameters may have changed), so default-profile sessions started
// after the update run the rebuilt instance.
func TestUpdate_SwapsFallback(t *testing.T) {
	r, builds := testRouter(t, map[string]string{}, nil) // everything → default

	oldDefault := r.For("anthropic", "", "", "s1")
	rebuilt := &fake{profile: "default-v2"}
	r.Update(func(string, string, string, string) string { return "default" }, rebuilt)

	if got := r.For("anthropic", "", "", "s2"); got != rebuilt {
		t.Errorf("new default-profile session got %q, want the rebuilt fallback", got.profile)
	}
	if got := r.For("anthropic", "", "", "s1"); got != oldDefault {
		t.Error("pre-update session must keep its original default instance")
	}
	if *builds != 0 {
		t.Errorf("default profile is fallback-seeded; Build called %d times, want 0", *builds)
	}
}

// TestFor_EmptySessionIDNoStick: session-less requests resolve
// against the current version every call and never grow the sticky
// map; the per-profile cache still prevents duplicate builds.
func TestFor_EmptySessionIDNoStick(t *testing.T) {
	r, builds := testRouter(t, map[string]string{"openai": "codex-safe"}, nil)

	c1 := r.For("openai", "", "", "")
	c2 := r.For("openai", "", "", "")
	if c1 != c2 {
		t.Error("same version, same profile: instances must match")
	}
	if *builds != 1 {
		t.Errorf("Build called %d times, want 1", *builds)
	}
	if n := len(r.sessions); n != 0 {
		t.Errorf("sticky map grew to %d on empty session IDs", n)
	}
}

// TestFor_BuildFailureFallsBackAndWarnsOnce: an unresolvable profile
// (assignment typo, future profile-file corruption) must degrade to
// the default instance without blocking, and warn exactly once per
// version — not once per request.
func TestFor_BuildFailureFallsBackAndWarnsOnce(t *testing.T) {
	var warns []string
	fallback := &fake{profile: "default"}
	r := New(Options[*fake]{
		Resolve:        func(string, string, string, string) string { return "broken" },
		Build:          func(name string) (*fake, error) { return nil, errors.New("boom") },
		DefaultProfile: "default",
		Fallback:       fallback,
		Warnf:          func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) },
	})

	got1 := r.For("anthropic", "", "", "s1")
	got2 := r.For("anthropic", "", "", "s2")
	if got1 != fallback || got2 != fallback {
		t.Error("failed build must fall back to the default instance")
	}
	if len(warns) != 1 {
		t.Fatalf("warned %d times, want 1: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], `"broken"`) {
		t.Errorf("warning should name the profile: %s", warns[0])
	}

	// Update resets the failed set: the (possibly fixed) profile is
	// retried and a still-broken one warns once more.
	r.Update(func(string, string, string, string) string { return "broken" }, fallback)
	_ = r.For("anthropic", "", "", "s3")
	if len(warns) != 2 {
		t.Errorf("post-Update retry: warned %d times total, want 2", len(warns))
	}
}

// TestStick_EvictsOldestAtCap mirrors cachetrack's LRU contract: the
// least-recently-touched session is evicted, and an evicted session
// re-resolves on next sight.
func TestStick_EvictsOldestAtCap(t *testing.T) {
	r, _ := testRouter(t, map[string]string{"anthropic": "claude-code"},
		func(o *Options[*fake]) { o.MaxSessions = 2 })

	r.For("anthropic", "", "", "s1")
	r.For("anthropic", "", "", "s2")
	r.For("anthropic", "", "", "s1") // refresh s1 — s2 becomes oldest
	r.For("anthropic", "", "", "s3") // evicts s2

	if _, ok := r.sessions["s2"]; ok {
		t.Error("s2 should have been evicted as least-recently touched")
	}
	for _, want := range []string{"s1", "s3"} {
		if _, ok := r.sessions[want]; !ok {
			t.Errorf("%s should have survived eviction", want)
		}
	}
	if n := len(r.sessions); n != 2 {
		t.Errorf("sticky map size %d, want 2", n)
	}
}

// TestOnResolve_FiresOncePerSession: the debug/evidence hook fires at
// first resolution only — sticky hits and session-less requests stay
// silent.
func TestOnResolve_FiresOncePerSession(t *testing.T) {
	var seen []string
	r, _ := testRouter(t, map[string]string{"openai": "codex-safe"},
		func(o *Options[*fake]) {
			o.OnResolve = func(provider, tool, cwd, sessionID, key string) {
				seen = append(seen, provider+"/"+tool+"/"+sessionID+"/"+key)
			}
		})

	r.For("openai", "", "", "s1")
	r.For("openai", "", "", "s1")
	r.For("openai", "", "", "")

	want := []string{"openai//s1/codex-safe"}
	if len(seen) != 1 || seen[0] != want[0] {
		t.Errorf("OnResolve calls: got %v want %v", seen, want)
	}
}

// TestFor_ConcurrentSmoke: parallel callers across providers and
// sessions must not race or duplicate builds (mutex-singleflight).
// The -race CI run is the real referee; this pins the build count.
func TestFor_ConcurrentSmoke(t *testing.T) {
	r, builds := testRouter(t, map[string]string{"anthropic": "claude-code", "openai": "codex-safe"}, nil)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provider := "anthropic"
			if i%2 == 0 {
				provider = "openai"
			}
			r.For(provider, "", "", fmt.Sprintf("sess-%d", i%8))
		}(i)
	}
	wg.Wait()
	if *builds > 2 {
		t.Errorf("Build called %d times under concurrency, want ≤2", *builds)
	}
}
