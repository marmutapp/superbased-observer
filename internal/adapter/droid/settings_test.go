package droid

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestSettingsPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/a/b/uuid.jsonl", "/a/b/uuid" + settingsSuffix},
		{"/a/b/uuid.settings.json", ""},
		{"/a/b/uuid", ""},
	}
	for _, tc := range cases {
		if got := settingsPath(tc.in); got != tc.want {
			t.Errorf("settingsPath(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadSidecarMissing(t *testing.T) {
	if _, _, ok := readSidecar(filepath.Join(t.TempDir(), "nope.jsonl")); ok {
		t.Error("readSidecar reported ok for a missing sidecar")
	}
}

func TestReadSidecarFields(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "s.jsonl")
	body := `{
	  "model": "claude-opus-5",
	  "providerLock": "anthropic",
	  "tokenUsage": {"inputTokens":10,"outputTokens":2,"cacheCreationTokens":3,"cacheReadTokens":40,"thinkingTokens":1,"factoryCredits":7},
	  "inclusiveTokenUsage": {"inputTokens":999,"outputTokens":999},
	  "childInclusiveTokenUsageBySessionId": {"child-1": {"inputTokens":900}},
	  "lastCallTokenUsage": {"inputTokens":5,"outputTokens":1,"cacheReadTokens":30}
	}`
	if err := os.WriteFile(filepath.Join(dir, "s"+settingsSuffix), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, mod, ok := readSidecar(transcript)
	if !ok {
		t.Fatal("readSidecar not ok")
	}
	if mod.IsZero() {
		t.Error("modtime is zero")
	}
	if sc.Model != "claude-opus-5" || sc.ProviderLock != "anthropic" {
		t.Errorf("model/providerLock=%q/%q", sc.Model, sc.ProviderLock)
	}
	if len(sc.ChildInclusiveTokenUsageBySessionID) != 1 {
		t.Errorf("child map=%v want one entry", sc.ChildInclusiveTokenUsageBySessionID)
	}

	ev, ok := tokenEvent(sc, transcript, "sess-1", "/proj", "main", time.Unix(1700000000, 0).UTC())
	if !ok {
		t.Fatal("tokenEvent not ok")
	}
	// The SELF-ONLY tokenUsage block is what ships — never
	// inclusiveTokenUsage (which would double-count child sessions) and
	// never lastCallTokenUsage (a subset).
	if ev.InputTokens != 10 || ev.OutputTokens != 2 {
		t.Errorf("input/output=%d/%d want 10/2 (tokenUsage, not inclusive/lastCall)", ev.InputTokens, ev.OutputTokens)
	}
	if ev.CacheCreationTokens != 3 || ev.CacheReadTokens != 40 || ev.ReasoningTokens != 1 {
		t.Errorf("cacheCreate/cacheRead/reasoning=%d/%d/%d want 3/40/1",
			ev.CacheCreationTokens, ev.CacheReadTokens, ev.ReasoningTokens)
	}
	if ev.SourceEventID != "tokens:sess-1" {
		t.Errorf("SourceEventID=%q want the stable per-session id", ev.SourceEventID)
	}
	if ev.ProjectRoot != "/proj" || ev.GitBranch != "main" {
		t.Errorf("projectRoot/branch=%q/%q", ev.ProjectRoot, ev.GitBranch)
	}
	if ev.Source != models.TokenSourceJSONL || ev.Reliability != models.ReliabilityApproximate {
		t.Errorf("source/reliability=%q/%q", ev.Source, ev.Reliability)
	}
	if ev.EstimatedCostUSD != 0 {
		t.Errorf("EstimatedCostUSD=%v want 0 (factoryCredits is not a cost)", ev.EstimatedCostUSD)
	}
}

// TestTokenEventInputNotReNetted pins the GROSS-vs-NET decision: droid
// already persists input NET of cacheRead (input < cacheRead is only
// possible when it is), so subtracting again would zero it out.
func TestTokenEventInputNotReNetted(t *testing.T) {
	sc := &sidecar{TokenUsage: tokenBlock{InputTokens: 3131, CacheReadTokens: 23040, OutputTokens: 69}}
	ev, ok := tokenEvent(sc, "/f.jsonl", "s", "", "", time.Time{})
	if !ok {
		t.Fatal("tokenEvent not ok")
	}
	if ev.InputTokens != 3131 {
		t.Errorf("InputTokens=%d want 3131 verbatim (re-netting would clamp it to 0)", ev.InputTokens)
	}
	if ev.CacheReadTokens != 23040 {
		t.Errorf("CacheReadTokens=%d want 23040", ev.CacheReadTokens)
	}
}

func TestTokenEventSuppressed(t *testing.T) {
	cases := []struct {
		name      string
		sc        *sidecar
		sessionID string
	}{
		{"nil sidecar", nil, "s"},
		{"no session id", &sidecar{TokenUsage: tokenBlock{InputTokens: 1}}, ""},
		{"all-zero usage", &sidecar{TokenUsage: tokenBlock{FactoryCredits: 5}}, "s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tokenEvent(tc.sc, "/f.jsonl", tc.sessionID, "", "", time.Time{}); ok {
				t.Error("tokenEvent emitted a row it should have suppressed")
			}
		})
	}
}

// TestReadSidecarRefusesSymlink pins the sidecar symlink guard. The
// `<uuid>.settings.json` path is DERIVED from the transcript name, so a
// symlink planted there would redirect the read at any file under
// ~/.factory the package doc promises is never read — settings.json
// holds plaintext BYOK API keys. A symlinked sidecar must behave exactly
// like a missing one: no read, no error, no content anywhere in the
// emitted events.
func TestReadSidecarRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, ".factory", "sessions", "-tmp-proj")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stand in for ~/.factory/settings.json: a global config carrying a
	// plaintext BYOK key, shaped so a naive read would surface it.
	globalCfg := filepath.Join(dir, ".factory", "settings.json")
	liveKey := "sk-" + "factory-" + "LIVE-CREDENTIAL-9f2b34e3"
	body := `{"model":"` + liveKey + `","tokenUsage":{"inputTokens":7,"outputTokens":9},` +
		`"customModels":[{"apiKey":"` + liveKey + `"}]}`
	if err := os.WriteFile(globalCfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	transcript := filepath.Join(sessions, "aaaa-bbbb.jsonl")
	line := `{"type":"session_start","id":"sess-1","title":"t","cwd":"/tmp/proj"}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(globalCfg, filepath.Join(sessions, "aaaa-bbbb"+settingsSuffix)); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	if _, _, ok := readSidecar(transcript); ok {
		t.Fatal("readSidecar followed a symlinked sidecar")
	}

	a := NewWithOptions(nil, filepath.Join(dir, ".factory", "sessions"))
	res, err := a.ParseSessionFile(context.Background(), transcript, 0)
	if err != nil {
		t.Fatalf("a symlinked sidecar must be a silent no-op, got: %v", err)
	}
	if len(res.TokenEvents) != 0 {
		t.Errorf("token events = %d, want 0 — the symlink target was read", len(res.TokenEvents))
	}
	if strings.Contains(renderEvents(res), liveKey) {
		t.Error("the symlink target's contents leaked into an emitted field")
	}
}

// renderEvents flattens every string field of a parse result so a test
// can assert that a secret appears NOWHERE.
func renderEvents(res adapter.ParseResult) string {
	var b strings.Builder
	for _, ev := range res.ToolEvents {
		b.WriteString(strings.Join([]string{
			ev.Target, ev.RawToolName, ev.RawToolInput, ev.ToolOutput,
			ev.ErrorMessage, ev.PrecedingReasoning, ev.Model, ev.SourceEventID,
		}, "\x00"))
	}
	for _, ev := range res.TokenEvents {
		b.WriteString(ev.Model + "\x00" + ev.SourceEventID)
	}
	b.WriteString(strings.Join(res.Warnings, "\x00"))
	return b.String()
}
