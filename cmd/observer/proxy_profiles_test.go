package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestNewCompressionRouter_PerProviderPipelines pins the Track-R
// assembly end to end at the cmd seam: under the baked default
// assignments, Anthropic and OpenAI traffic get DISTINCT pipeline
// instances (claude-code vs codex-safe parameters), while sessions on
// the same provider share one immutable instance — the §2.2.1
// cross-tool defect fix in miniature.
func TestNewCompressionRouter_PerProviderPipelines(t *testing.T) {
	cfg := config.Default()
	cfg.Compression.Conversation.Enabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	loadFresh := func() (config.Config, error) { return cfg, nil }
	router, authCache, reload := newCompressionRouter(cfg, "", loadFresh, nil, logger)
	if authCache != nil {
		t.Error("rolling disabled: shared auth cache must be nil")
	}
	if reload == nil {
		t.Fatal("hot-reload hook must always be returned")
	}

	anth := router.r.For(models.ProviderAnthropic, "", "", "sess-a")
	oai := router.r.For(models.ProviderOpenAI, "", "", "sess-b")
	if anth == oai {
		t.Error("anthropic and openai sessions must run distinct profile pipelines")
	}
	if again := router.r.For(models.ProviderAnthropic, "", "", "sess-c"); again != anth {
		t.Error("same profile must reuse one pipeline instance across sessions")
	}

	// Smoke the actual compressor contract: a minimal body round-trips
	// through CompressInSession without panicking on a nil-DB,
	// rolling-off, stash-off assembly.
	res := router.CompressInSession(context.Background(), models.ProviderAnthropic,
		[]byte(`{"model":"claude-opus-4-8","messages":[]}`), "sess-a")
	if !res.Skipped && res.Body == nil {
		t.Error("compressor returned neither Skipped nor a body")
	}
}

// TestApplyRecipeAliasToProfiles pins the P2.2 deprecation contract:
// `--recipe <name>` maps to a run-scoped default-profile assignment
// for ALL traffic (per-provider table cleared), unknown names stay a
// loud error (the old flag's behavior), and empty is a no-op.
func TestApplyRecipeAliasToProfiles(t *testing.T) {
	cfg := config.Default()
	if err := applyRecipeAliasToProfiles(&cfg, "codex-safe"); err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles.Default != "codex-safe" {
		t.Errorf("default profile: got %q want codex-safe", cfg.Profiles.Default)
	}
	if len(cfg.Profiles.ByProvider) != 0 {
		t.Errorf("per-provider table must be cleared (daemon-wide overlay equivalence), got %v", cfg.Profiles.ByProvider)
	}
	if got := config.ResolveProfileName(cfg.Profiles, models.ProviderAnthropic, ""); got != "codex-safe" {
		t.Errorf("anthropic traffic under the alias: got %q want codex-safe", got)
	}

	cfg2 := config.Default()
	if err := applyRecipeAliasToProfiles(&cfg2, "no-such-recipe"); err == nil {
		t.Error("unknown recipe name must error loudly, like the old --recipe flag")
	}

	cfg3 := config.Default()
	if err := applyRecipeAliasToProfiles(&cfg3, ""); err != nil {
		t.Fatal(err)
	}
	if cfg3.Profiles.Default != config.Default().Profiles.Default {
		t.Error("empty alias must be a no-op")
	}
}

// TestNewCompressionRouter_HotReload pins P2.5 at the cmd seam: a
// config.toml edit + reload() re-points NEW sessions while existing
// sessions keep their sticky instances; a failed re-load (corrupt
// file) keeps current assignments instead of breaking traffic.
func TestNewCompressionRouter_HotReload(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	loadFresh := func() (config.Config, error) {
		return config.Load(config.LoadOptions{GlobalPath: configPath})
	}
	cfg, err := loadFresh()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Compression.Conversation.Enabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	router, _, reload := newCompressionRouter(cfg, "", loadFresh, nil, logger)
	before := router.r.For(models.ProviderAnthropic, "", "", "sess-old")

	// Re-assign anthropic traffic and reload.
	body := "[profiles.by_provider]\nanthropic = \"codex-variant\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reload()

	if got := router.r.For(models.ProviderAnthropic, "", "", "sess-old"); got != before {
		t.Error("existing session must keep its instance across hot reload")
	}
	after := router.r.For(models.ProviderAnthropic, "", "", "sess-new")
	if after == before {
		t.Error("new session must get a freshly built instance post-reload")
	}

	// A corrupt config must not disturb the router.
	if err := os.WriteFile(configPath, []byte("not [valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	reload()
	if got := router.r.For(models.ProviderAnthropic, "", "", "sess-new2"); got != after {
		t.Error("failed reload must keep the current (post-first-reload) assignments")
	}
}

// TestNewCompressionRouter_ProjectOverride pins R3 at the cmd seam:
// a session whose CWD sits under a repo with .observer/config.toml
// gets its own pipeline instance (project-scoped key), the project's
// [profiles] assignment overrides the global table, and a repo-file
// edit re-resolves NEW sessions while in-flight ones stay sticky.
func TestNewCompressionRouter_ProjectOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Compression.Conversation.Enabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loadFresh := func() (config.Config, error) { return cfg, nil }
	router, _, _ := newCompressionRouter(cfg, "", loadFresh, nil, logger)

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".observer"), 0o755); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(repo, ".observer", "config.toml")
	if err := os.WriteFile(overlay, []byte("[compression.conversation]\ntarget_ratio = 0.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(repo, "src")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	plain := router.r.For(models.ProviderAnthropic, "", "", "sess-plain")
	scoped := router.r.For(models.ProviderAnthropic, "", cwd, "sess-scoped")
	if plain == scoped {
		t.Error("project-scoped session must get its own pipeline instance")
	}
	if again := router.r.For(models.ProviderAnthropic, "", cwd, "sess-scoped2"); again != scoped {
		t.Error("same project + profile must share one instance")
	}

	// Repo-file edit: new sessions rebuild (stamp in key), sticky
	// sessions keep theirs.
	if err := os.WriteFile(overlay, []byte("[compression.conversation]\ntarget_ratio = 0.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(overlay, future, future); err != nil {
		t.Fatal(err)
	}
	if got := router.r.For(models.ProviderAnthropic, "", cwd, "sess-scoped"); got != scoped {
		t.Error("in-flight session must stay sticky across a repo-file edit")
	}
	edited := router.r.For(models.ProviderAnthropic, "", cwd, "sess-post-edit")
	if edited == scoped {
		t.Error("post-edit session must get a freshly built instance")
	}
}
