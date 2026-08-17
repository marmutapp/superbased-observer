package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// warnCounter is a minimal slog.Handler that counts WARN records whose message
// contains a substring, so the test asserts on the LOG DECISION rather than on
// formatting.
type warnCounter struct {
	mu    sync.Mutex
	want  string
	count int
	buf   bytes.Buffer
}

func (w *warnCounter) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelWarn }

func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.Contains(r.Message, w.want) {
		w.count++
		w.buf.WriteString(r.Message + "\n")
	}
	return nil
}

func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(string) slog.Handler      { return w }

func (w *warnCounter) n() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// newWarnCountingProxy builds a proxy whose logger counts the unknown-upstream
// warning. upstreams may be nil (the no-[proxy.upstreams] case).
func newWarnCountingProxy(t *testing.T, upstreams map[string]string) (*Proxy, *warnCounter) {
	t.Helper()
	wc := &warnCounter{want: "unknown /up/ upstream id"}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		OpenAIUpstream:    "https://api.openai.com",
		Upstreams:         upstreams,
		Sink:              &fakeSink{},
		Logger:            slog.New(wc),
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, wc
}

// TestStripUpstreamPrefixWarnsOncePerUnknownID pins the warn-once dedup: an
// unknown `/up/<id>/` id is reported exactly ONCE no matter how many requests
// carry it, and a second distinct id gets its own single warning. Without the
// dedup a misconfigured launcher (`--upstream` typo) sprays one WARN per turn.
func TestStripUpstreamPrefixWarnsOncePerUnknownID(t *testing.T) {
	p, wc := newWarnCountingProxy(t, map[string]string{"openrouter": "https://openrouter.ai/api"})

	for i := 0; i < 50; i++ {
		r := httptest.NewRequest(http.MethodPost, "/up/typo/v1/chat/completions", nil)
		if up, _ := p.stripUpstreamPrefix(r); up != nil {
			t.Fatalf("unknown id must not resolve an upstream")
		}
		if r.URL.Path != "/up/typo/v1/chat/completions" {
			t.Fatalf("fail-open broken: path rewritten to %q", r.URL.Path)
		}
	}
	if got := wc.n(); got != 1 {
		t.Errorf("50 identical unknown ids produced %d warns, want exactly 1", got)
	}

	r := httptest.NewRequest(http.MethodPost, "/up/other/v1/chat/completions", nil)
	if up, _ := p.stripUpstreamPrefix(r); up != nil {
		t.Fatalf("second unknown id must not resolve an upstream")
	}
	if got := wc.n(); got != 2 {
		t.Errorf("a second DISTINCT unknown id produced %d warns total, want 2", got)
	}
}

// TestStripUpstreamPrefixSilentBranches pins the warning's SELECTIVITY: every
// non-unknown-id branch stays completely silent. A warning here would fire on
// ordinary traffic (every request that has no /up/ prefix at all).
func TestStripUpstreamPrefixSilentBranches(t *testing.T) {
	cases := []struct {
		name      string
		upstreams map[string]string
		path      string
	}{
		{"known id", map[string]string{"openrouter": "https://openrouter.ai/api"}, "/up/openrouter/api/v1/chat/completions"},
		{"no /up/ prefix", map[string]string{"openrouter": "https://openrouter.ai/api"}, "/v1/chat/completions"},
		{"prefix with no trailing segment", map[string]string{"openrouter": "https://openrouter.ai/api"}, "/up/openrouter"},
		{"empty id", map[string]string{"openrouter": "https://openrouter.ai/api"}, "/up//v1"},
		{"no upstreams configured", nil, "/up/openrouter/api/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, wc := newWarnCountingProxy(t, tc.upstreams)
			for i := 0; i < 20; i++ {
				p.stripUpstreamPrefix(httptest.NewRequest(http.MethodPost, tc.path, nil))
			}
			if got := wc.n(); got != 0 {
				t.Errorf("%s produced %d warns, want 0:\n%s", tc.name, got, wc.buf.String())
			}
		})
	}
}

// TestStripUpstreamPrefixWarnCapBoundsTheDedupMap pins the bound: a long-lived
// daemon sprayed with distinct unknown ids warns at most unknownUpstreamWarnCap
// times and keeps the dedup map at that size, so it cannot grow without limit.
func TestStripUpstreamPrefixWarnCapBoundsTheDedupMap(t *testing.T) {
	p, wc := newWarnCountingProxy(t, map[string]string{"openrouter": "https://openrouter.ai/api"})

	const distinct = 500
	for i := 0; i < distinct; i++ {
		path := fmt.Sprintf("/up/id-%d/v1/chat/completions", i)
		if up, _ := p.stripUpstreamPrefix(httptest.NewRequest(http.MethodPost, path, nil)); up != nil {
			t.Fatalf("unknown id %d must not resolve an upstream", i)
		}
	}
	if got := wc.n(); got != unknownUpstreamWarnCap {
		t.Errorf("%d distinct unknown ids produced %d warns, want %d (the cap)", distinct, got, unknownUpstreamWarnCap)
	}
	if got := mapLen(&p.unknownUpstreamWarned); got != unknownUpstreamWarnCap {
		t.Errorf("dedup map holds %d entries, want %d (the cap bounds the STORE, not just the log)", got, unknownUpstreamWarnCap)
	}
	// Routing behaviour is untouched by the cap: a known id still resolves.
	r := httptest.NewRequest(http.MethodPost, "/up/openrouter/api/v1/chat/completions", nil)
	if up, _ := p.stripUpstreamPrefix(r); up == nil {
		t.Error("known id stopped resolving after the warn cap was reached")
	} else if r.URL.Path != "/api/v1/chat/completions" {
		t.Errorf("known id path = %q, want /api/v1/chat/completions", r.URL.Path)
	}
}
