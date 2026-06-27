package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	t.Parallel()
	if err := Validate(Default()); err != nil {
		t.Fatalf("Default() failed validation: %v", err)
	}
}

// TestOTelExporterDefaultsAreOff pins the solo-local invariant for the M4
// exporter: a default config leaves [exporter.otel] disabled (so start.go
// starts no exporter goroutine and the daemon makes zero OTLP calls) while the
// non-gating fields carry the documented privacy-preserving defaults.
func TestOTelExporterDefaultsAreOff(t *testing.T) {
	t.Parallel()
	o := Default().Exporter.OTel
	if o.Enabled {
		t.Error("Exporter.OTel.Enabled must default to false (solo-local invariant)")
	}
	if o.EmitPromptContent {
		t.Error("EmitPromptContent must default to false")
	}
	if o.EmitUserEmail {
		t.Error("EmitUserEmail must default to false")
	}
	if o.Endpoint != DefaultOTelEndpoint {
		t.Errorf("Endpoint: got %q want %q", o.Endpoint, DefaultOTelEndpoint)
	}
	if o.PollIntervalSeconds != DefaultOTelPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds: got %d want %d", o.PollIntervalSeconds, DefaultOTelPollIntervalSeconds)
	}
	if o.SemconvStability != DefaultOTelSemconvStability {
		t.Errorf("SemconvStability: got %q want %q", o.SemconvStability, DefaultOTelSemconvStability)
	}
}

// TestDefaultCompressTypesIsJSONLogsCode pins V7-24 (v1.7.23, 2026-06-01).
// The default `compress_types` is restored to ["json","logs","code"]
// after V7-22's temporary {} flip was re-measured on the V7-22 binary
// at n=8 and found to be safe — V7-22's preceding fixes (V7-19 nil-trap
// + V7-21 tools-defs gate) closed enough of the re-marshal pathway that
// per-type compression no longer cascades on the V7-22+ binary.
//
// Empirical headline (n=8 vs n=4 OFF on V7-22 binary):
//
//	OFF (no proxy):                $1.148 / 15.0 turns  CV 7.5%
//	B proxy, compress_types=[]:    $1.118 / 15.5 turns  CV 9.1%  -2.6%
//	B proxy, this default:         $1.069 / 14.9 turns  CV 7.6%  -6.9%
//
// "text" is omitted by choice — TextCompressor head-tail elision is
// the v1.4.38 regression class. "tools" is opt-in (V7-21). Stash is
// opt-in and cache-breaking (V7-24).
//
// Operators MUST set ENABLE_TOOL_SEARCH=true in the launching shell
// for the proxy to be a net win — without it the Claude Code SDK
// eager-inlines MCP schemas (~+21K tokens/turn) under ANTHROPIC_BASE_URL.
//
// See docs/v1.7.23-compression-savings-empirical-2026-06-01.md.
//
// History:
//
//	pre-v1.4.40: ["json","logs"]
//	v1.4.40:     ["json","logs","code"] (CodeCompressor became content-preserving)
//	v1.7.22:     []                     (V7-22 flip — V7-21 binary cascade)
//	v1.7.23:     ["json","logs","code"] (V7-24 — V7-22 binary cascade resolved)
func TestDefaultCompressTypesIsJSONLogsCode(t *testing.T) {
	t.Parallel()
	got := Default().Compression.Conversation.CompressTypes
	want := []string{"json", "logs", "code"}
	if len(got) != len(want) {
		t.Errorf("CompressTypes: got %v, want %v (V7-24)", got, want)
		return
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("CompressTypes[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

func TestLoadAppliesGlobalTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	body := `
[observer]
log_level = "debug"

[observer.watch]
max_file_size_mb = 200

[proxy]
port = 9999
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: p, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.LogLevel != "debug" {
		t.Errorf("log_level: got %q, want %q", cfg.Observer.LogLevel, "debug")
	}
	if cfg.Observer.Watch.MaxFileSizeMB != 200 {
		t.Errorf("max_file_size_mb: got %d, want 200", cfg.Observer.Watch.MaxFileSizeMB)
	}
	if cfg.Proxy.Port != 9999 {
		t.Errorf("proxy.port: got %d, want 9999", cfg.Proxy.Port)
	}
	// Unrelated defaults preserved.
	if !cfg.Compression.Shell.Enabled {
		t.Error("compression.shell.enabled should default true")
	}
}

// TestEmptyEnabledAdaptersDisablesWatch pins the explicit-empty-list
// semantics. A user who writes `enabled_adapters = []` in
// config.toml expects the watcher to skip every adapter — not to
// silently fall through to Default()'s populated list.
// BurntSushi/toml correctly preserves the nil vs. non-nil-empty
// distinction (key absent → leaves Default() alone, key present
// with `[]` → overwrites with []string{}). Pair this with the
// registry.Detected nil-vs-empty fix in internal/adapter/registry.go.
func TestEmptyEnabledAdaptersDisablesWatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	body := `
[observer.watch]
enabled_adapters = []
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: p, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.Watch.EnabledAdapters == nil {
		t.Fatal("EnabledAdapters is nil — expected non-nil empty slice (the registry distinguishes these)")
	}
	if len(cfg.Observer.Watch.EnabledAdapters) != 0 {
		t.Errorf("EnabledAdapters: got %v, want non-nil empty slice", cfg.Observer.Watch.EnabledAdapters)
	}
}

func TestProjectOverridesGlobal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gp := filepath.Join(dir, "global.toml")
	pp := filepath.Join(dir, "project.toml")
	if err := os.WriteFile(gp, []byte(`[observer]
log_level = "warn"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte(`[observer]
log_level = "debug"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: gp, ProjectPath: pp, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.LogLevel != "debug" {
		t.Errorf("project should override global: got %q", cfg.Observer.LogLevel)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"OBSERVER_OBSERVER_LOG_LEVEL":               "warn",
		"OBSERVER_PROXY_PORT":                       "1234",
		"OBSERVER_COMPRESSION_CONVERSATION_ENABLED": "true",
		"OBSERVER_OBSERVER_WATCH_ENABLED_ADAPTERS":  "claude-code,codex",
	}
	cfg, err := Load(LoadOptions{GlobalPath: "", Env: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.LogLevel != "warn" {
		t.Errorf("log_level: got %q", cfg.Observer.LogLevel)
	}
	if cfg.Proxy.Port != 1234 {
		t.Errorf("port: got %d", cfg.Proxy.Port)
	}
	if !cfg.Compression.Conversation.Enabled {
		t.Errorf("conversation.enabled not overridden")
	}
	if got, want := cfg.Observer.Watch.EnabledAdapters, []string{"claude-code", "codex"}; !equalSlice(got, want) {
		t.Errorf("enabled_adapters: got %v want %v", got, want)
	}
}

func TestValidateRejectsBadLogLevel(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Observer.LogLevel = "trace"
	if err := Validate(c); err == nil {
		t.Fatal("expected error for log_level=trace")
	}
}

func TestValidateRejectsBadConversationMode(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Compression.Conversation.Enabled = true
	c.Compression.Conversation.Mode = "sillystring"
	if err := Validate(c); err == nil {
		t.Fatal("expected error for conversation.mode=sillystring")
	}
}

func TestMissingGlobalFileIsNotAnError(t *testing.T) {
	t.Parallel()
	cfg, err := Load(LoadOptions{
		GlobalPath: filepath.Join(t.TempDir(), "does-not-exist.toml"),
		Env:        func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.LogLevel != "info" {
		t.Errorf("expected default log_level, got %q", cfg.Observer.LogLevel)
	}
}

func TestExpandHome(t *testing.T) {
	t.Parallel()
	cfg, err := Load(LoadOptions{GlobalPath: "", Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observer.DBPath == "~/.observer/observer.db" {
		t.Errorf("DBPath not expanded: %s", cfg.Observer.DBPath)
	}
}

// TestDefaultLogsConfig pins the v1.7.6 LogsCompressor defaults
// (MaxLines=200, Head=100, Tail=100) — same numbers as the
// prior hardcoded NewLogsCompressor constructor, now operator-tunable
// per docs/codex-compression-recipe.md.
func TestDefaultLogsConfig(t *testing.T) {
	t.Parallel()
	logs := Default().Compression.Conversation.Logs
	if logs.MaxLines != 200 {
		t.Errorf("MaxLines: got %d want 200", logs.MaxLines)
	}
	if logs.Head != 100 {
		t.Errorf("Head: got %d want 100", logs.Head)
	}
	if logs.Tail != 100 {
		t.Errorf("Tail: got %d want 100", logs.Tail)
	}
}

// TestValidateAcceptsZeroMaxLines pins that MaxLines=0 (the
// "disable truncation" sentinel) passes validation — this is the
// codex-variant recipe's setting.
func TestValidateAcceptsZeroMaxLines(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Compression.Conversation.Logs.MaxLines = 0
	c.Compression.Conversation.Logs.Head = 0
	c.Compression.Conversation.Logs.Tail = 0
	if err := Validate(c); err != nil {
		t.Fatalf("MaxLines=0 must be valid: %v", err)
	}
}

// TestValidateRejectsNegativeLogsKnobs pins the contract that
// MaxLines/Head/Tail must be >= 0.
func TestValidateRejectsNegativeLogsKnobs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"negative_max_lines", func(c *Config) { c.Compression.Conversation.Logs.MaxLines = -1 }},
		{"negative_head", func(c *Config) { c.Compression.Conversation.Logs.Head = -1 }},
		{"negative_tail", func(c *Config) { c.Compression.Conversation.Logs.Tail = -1 }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := Default()
			tc.mutate(&c)
			if err := Validate(c); err == nil {
				t.Fatalf("Validate %s: expected error, got nil", tc.name)
			}
		})
	}
}

// TestLoadLogsConfigRoundtrips pins that TOML round-trips through
// [Load] into the LogsConfig fields. The codex-variant recipe's
// `max_lines = 0` is the load-bearing case (recipe in
// docs/recipes/codex-variant.toml relies on this).
func TestLoadLogsConfigRoundtrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "observer.toml")
	body := []byte(`
[compression.conversation.logs]
max_lines = 500
head      = 250
tail      = 250
`)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: cfgPath, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Compression.Conversation.Logs.MaxLines; got != 500 {
		t.Errorf("MaxLines: got %d want 500", got)
	}
	if got := cfg.Compression.Conversation.Logs.Head; got != 250 {
		t.Errorf("Head: got %d want 250", got)
	}
	if got := cfg.Compression.Conversation.Logs.Tail; got != 250 {
		t.Errorf("Tail: got %d want 250", got)
	}
}

// TestDefaultStashMaxTotalMBIs1024 pins the v1.7.6 stash cap bump
// (256 → 1024 MB) per V7-13 Gap 2 (ii). Code-agent workloads churn
// large file reads at a rate that fills 256 MB fast; 1024 is the
// new working-set cap.
func TestDefaultStashMaxTotalMBIs1024(t *testing.T) {
	t.Parallel()
	got := Default().Compression.Conversation.Stash.MaxTotalMB
	if got != 1024 {
		t.Errorf("Stash.MaxTotalMB: got %d want 1024", got)
	}
}

// TestIntelligenceMCPGetFile_DefaultsAreSafe pins the v1.7.8
// [intelligence.mcp.get_file] defaults: enabled, 100 KB cap, deny
// patterns covering the standard secrets/SCM/dep paths.
func TestIntelligenceMCPGetFile_DefaultsAreSafe(t *testing.T) {
	t.Parallel()
	got := Default().Intelligence.MCP.GetFile
	if !got.Enabled {
		t.Errorf("GetFile.Enabled: want true (V7-12 ships ON by default)")
	}
	if got.MaxResponseKB != 100 {
		t.Errorf("GetFile.MaxResponseKB: got %d want 100", got.MaxResponseKB)
	}
	want := []string{".env*", ".git/**", "node_modules/**", ".ssh/**", "*.key"}
	denyIdx := map[string]bool{}
	for _, p := range got.DenyPaths {
		denyIdx[p] = true
	}
	for _, p := range want {
		if !denyIdx[p] {
			t.Errorf("GetFile.DenyPaths missing canonical default %q", p)
		}
	}
	if !Default().Intelligence.MCP.Audit.Enabled {
		t.Errorf("Audit.Enabled: want true (forensic value high, opt-out via TOML)")
	}
}

// TestIntelligenceMCPGetSymbols_DefaultsAreSane pins the v1.7.9
// [intelligence.mcp.get_symbols] defaults: enabled, 20/20 caller/
// callee caps (V7-12 spec).
func TestIntelligenceMCPGetSymbols_DefaultsAreSane(t *testing.T) {
	t.Parallel()
	got := Default().Intelligence.MCP.GetSymbols
	if !got.Enabled {
		t.Errorf("GetSymbols.Enabled: want true (V7-12 ships ON by default)")
	}
	if got.MaxCallers != 20 {
		t.Errorf("GetSymbols.MaxCallers: got %d want 20", got.MaxCallers)
	}
	if got.MaxCallees != 20 {
		t.Errorf("GetSymbols.MaxCallees: got %d want 20", got.MaxCallees)
	}
}

// TestIntelligenceMCPGetRelations_DefaultsAreSane pins the v1.7.10
// [intelligence.mcp.get_relations] defaults: enabled, depth 5 cap,
// 100-result cap (V7-12 spec).
func TestIntelligenceMCPGetRelations_DefaultsAreSane(t *testing.T) {
	t.Parallel()
	got := Default().Intelligence.MCP.GetRelations
	if !got.Enabled {
		t.Errorf("GetRelations.Enabled: want true (V7-12 ships ON by default)")
	}
	if got.MaxDepth != 5 {
		t.Errorf("GetRelations.MaxDepth: got %d want 5", got.MaxDepth)
	}
	if got.MaxResults != 100 {
		t.Errorf("GetRelations.MaxResults: got %d want 100", got.MaxResults)
	}
}

// TestIntelligenceMCPRetrieveStashed_DefaultsAreSane pins the
// v1.7.11 [intelligence.mcp.retrieve_stashed] defaults: enabled, 25
// shas per call (matches get_symbols's batch cap).
func TestIntelligenceMCPRetrieveStashed_DefaultsAreSane(t *testing.T) {
	t.Parallel()
	got := Default().Intelligence.MCP.RetrieveStashed
	if !got.Enabled {
		t.Errorf("RetrieveStashed.Enabled: want true (ships ON by default)")
	}
	if got.MaxShasPerCall != 25 {
		t.Errorf("RetrieveStashed.MaxShasPerCall: got %d want 25", got.MaxShasPerCall)
	}
}

// TestIntelligenceMCPFeatures_DefaultIsEmpty pins the V7-16 default:
// Features is an empty slice (NOT nil) so TOML round-trips emit
// `features = []` consistently. Empty list semantically = "no filter
// applied"; non-empty becomes a strict allow-list scoped to the four
// V7-12 tools.
func TestIntelligenceMCPFeatures_DefaultIsEmpty(t *testing.T) {
	t.Parallel()
	got := Default().Intelligence.MCP.Features
	if got == nil {
		t.Errorf("Features: got nil, want []string{} (round-trip stability)")
	}
	if len(got) != 0 {
		t.Errorf("Features: got %v, want empty slice (no filter applied by default)", got)
	}
}

// TestUnsupportedDenyPatterns surfaces silently-dead patterns at
// startup. Called by `observer serve` to emit one warning per
// unsupported entry; this test pins the supported / unsupported
// syntax decision.
func TestUnsupportedDenyPatterns(t *testing.T) {
	t.Parallel()
	g := IntelligenceMCPGetFileConfig{
		DenyPaths: []string{
			".env*",        // supported
			"*.key",        // supported
			".git/**",      // supported
			"src/?oo.ts",   // supported
			"src/[ab].ts",  // unsupported — char class
			"src/{a,b}.ts", // unsupported — braces
			`src/\*.ts`,    // unsupported — escape
		},
	}
	bad := g.UnsupportedDenyPatterns()
	wantBad := map[string]bool{
		"src/[ab].ts":  true,
		"src/{a,b}.ts": true,
		`src/\*.ts`:    true,
	}
	if len(bad) != len(wantBad) {
		t.Fatalf("got %d unsupported patterns (%v), want %d", len(bad), bad, len(wantBad))
	}
	for _, p := range bad {
		if !wantBad[p] {
			t.Errorf("pattern %q reported as unsupported but should have been supported", p)
		}
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGuardDefaults pins the guard partial-merge invariant (guard
// spec SS16 + operator decision D2): an install with NO [guard]
// section gets Enabled=true + Mode="observe" from Default() -- never
// zero values -- and a PARTIAL [guard] section keeps every unset
// field's default (the cachetrack live-daemon-captures-nothing bug
// class). Cloud features stay all-off (D1).
func TestGuardDefaults(t *testing.T) {
	t.Parallel()
	g := Default().Guard
	if !g.Enabled || g.Mode != "observe" {
		t.Errorf("guard defaults = enabled=%v mode=%q, want true/observe (D2)", g.Enabled, g.Mode)
	}
	if g.Strict {
		t.Error("guard.strict must default false (Q2 fail-open)")
	}
	if g.RetentionDays != 365 {
		t.Errorf("guard.retention_days = %d, want 365", g.RetentionDays)
	}
	if g.Rules.UserPolicy != "~/.observer/guard-policy.toml" || g.Rules.ProjectPolicy != ".observer/guard-policy.toml" {
		t.Errorf("guard.rules policy paths = %q / %q", g.Rules.UserPolicy, g.Rules.ProjectPolicy)
	}
	if !g.Taint.Enabled || g.Taint.DecayTurns != 10 {
		t.Errorf("guard.taint = %+v, want enabled/10", g.Taint)
	}
	if g.Boundary.AllowPaths != nil || g.Boundary.ProtectedBranches != nil {
		t.Errorf("guard.boundary slices must default nil (engine defaults apply): %+v", g.Boundary)
	}
	if g.Cloud.Enabled || g.Cloud.LLMJudge.Enabled || g.Cloud.Reputation.Enabled {
		t.Error("guard.cloud features must ALL default off (D1)")
	}
	if g.Alerts.MinSeverity != "high" || !g.Alerts.Desktop {
		t.Errorf("guard.alerts = %+v, want desktop/high", g.Alerts)
	}

	// Partial-merge: a [guard] section setting ONLY mode keeps every
	// other default (Enabled stays true, taint stays on).
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[guard]\nmode = \"enforce\"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: p})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Guard.Mode != "enforce" {
		t.Errorf("mode = %q, want enforce", cfg.Guard.Mode)
	}
	if !cfg.Guard.Enabled || !cfg.Guard.Taint.Enabled || cfg.Guard.RetentionDays != 365 {
		t.Errorf("partial [guard] section lost defaults: %+v", cfg.Guard)
	}

	// Validate rejects the known-bad shapes loudly.
	bad := Default()
	bad.Guard.Mode = "bogus"
	if err := Validate(bad); err == nil {
		t.Error("Validate accepted guard.mode=bogus")
	}
	bad = Default()
	bad.Guard.Rules.CEL = true
	if err := Validate(bad); err == nil {
		t.Error("Validate accepted guard.rules.cel=true (deferred Q1)")
	}
}

// TestPredictDefaults pins the [predict] partial-merge invariant
// (docs/cost-predictor.md): default-ON with the tuning defaults, and a
// PARTIAL [predict] section keeps every unset field (the cachetrack
// live-daemon-captures-nothing bug class).
func TestPredictDefaults(t *testing.T) {
	t.Parallel()
	p := Default().Predict
	if !p.Enabled {
		t.Error("predict must default enabled=true")
	}
	if p.YoungSessionMessages != 3 || p.DefaultTurnsPerMessage != 12 || p.PriorWindowDays != 30 {
		t.Errorf("predict defaults = %+v, want 3/12/30", p)
	}

	// Partial-merge: a [predict] section setting ONLY enabled=false keeps
	// the other tuning defaults.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[predict]\nenabled = false\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Predict.Enabled {
		t.Error("enabled=false should override")
	}
	if cfg.Predict.YoungSessionMessages != 3 || cfg.Predict.DefaultTurnsPerMessage != 12 {
		t.Errorf("partial [predict] lost tuning defaults: %+v", cfg.Predict)
	}
}

// TestProcessObservabilityDefaults pins the opt-in posture of the
// [observer.process] surface (docs/process-observability.md §11): the
// feature defaults OFF (D1, unlike Guard/CacheTrack/Advisor), and the
// non-zero defaults are inherited on a partial-merge so flipping just
// `enabled = true` yields a sane backend/queue/argv config rather than
// zeros that would fail Validate.
func TestProcessObservabilityDefaults(t *testing.T) {
	t.Parallel()
	p := Default().Observer.Process
	if p.Enabled {
		t.Error("observer.process.enabled must default to false (D1 — opt-in feature)")
	}
	if p.Backend != "auto" {
		t.Errorf("backend = %q, want auto", p.Backend)
	}
	if p.CaptureUnattributed {
		t.Error("capture_unattributed must default false (D5/§9.2.7)")
	}
	if p.RetentionDays != 30 || p.QueueSize != 10000 || p.BatchSize != 250 {
		t.Errorf("retention/queue/batch = %d/%d/%d, want 30/10000/250", p.RetentionDays, p.QueueSize, p.BatchSize)
	}
	// Unified poll rate defaults to 2000ms; the bridge override defaults to 0
	// (inherit). The 0-default is load-bearing: the dashboard "process poll
	// rate" knob writes PollIntervalMS, and BridgePollIntervalMS=0 keeps the
	// bridge on the same cadence unless the operator deliberately splits them.
	if p.PollIntervalMS != 2000 {
		t.Errorf("poll_interval_ms = %d, want 2000", p.PollIntervalMS)
	}
	if p.BridgePollIntervalMS != 0 {
		t.Errorf("bridge_poll_interval_ms = %d, want 0 (inherit)", p.BridgePollIntervalMS)
	}
	if p.Argv.Mode != "preview" || p.Argv.MaxPreviewBytes != 512 || !p.Argv.StoreArgCount {
		t.Errorf("argv defaults = %+v, want preview/512/true", p.Argv)
	}
	if !p.Env.Enabled || !p.Env.StorePathHash {
		t.Errorf("env defaults = %+v, want enabled/path-hash", p.Env)
	}
	if p.Network.Enabled || p.Filesystem.Enabled || p.Executable.HashEnabled {
		t.Error("network/filesystem/executable-hash must ALL default off")
	}

	// Partial-merge: a section setting ONLY enabled keeps every other
	// default (backend stays auto, queue/batch/argv stay sane).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[observer.process]\nenabled = true\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: cfgPath, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Observer.Process
	if !got.Enabled {
		t.Error("enabled = false after explicitly setting it true")
	}
	if got.Backend != "auto" || got.QueueSize != 10000 || got.BatchSize != 250 || got.Argv.Mode != "preview" {
		t.Errorf("partial [observer.process] section lost defaults: %+v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("partial-merged process config failed Validate: %v", err)
	}

	// Validate rejects known-bad enums loudly — but ONLY when enabled.
	for _, mut := range []func(*Config){
		func(c *Config) { c.Observer.Process.Backend = "bogus" },
		func(c *Config) { c.Observer.Process.Argv.Mode = "bogus" },
		func(c *Config) { c.Observer.Process.Filesystem.Mode = "bogus" },
		func(c *Config) { c.Observer.Process.QueueSize = 0 },
		func(c *Config) { c.Observer.Process.PollIntervalMS = -1 },
		func(c *Config) { c.Observer.Process.BridgePollIntervalMS = -1 },
	} {
		bad := Default()
		bad.Observer.Process.Enabled = true
		mut(&bad)
		if err := Validate(bad); err == nil {
			t.Error("Validate accepted a known-bad enabled process config")
		}
	}
	// A bogus backend on a DISABLED section must NOT fail (opt-in: stale
	// sections never break the daemon).
	off := Default()
	off.Observer.Process.Backend = "bogus"
	if err := Validate(off); err != nil {
		t.Errorf("Validate rejected a disabled process config with a bad enum: %v", err)
	}

	// Every backend selectProcessBackend (cmd/observer/processobs.go) accepts
	// must pass Validate when enabled — in particular "bridge", the cross-OS
	// backend that "auto" resolves to on WSL. Regression guard: the validator
	// once omitted "bridge" and spuriously rejected an explicit
	// backend = "bridge" the docs/comment documented as valid.
	for _, backend := range []string{"auto", "bridge", "both", "poll", "linux_ebpf", "etw", "endpointsecurity", "off"} {
		ok := Default()
		ok.Observer.Process.Enabled = true
		ok.Observer.Process.Backend = backend
		if err := Validate(ok); err != nil {
			t.Errorf("Validate rejected valid enabled process backend %q: %v", backend, err)
		}
	}
}
