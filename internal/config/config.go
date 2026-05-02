package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the root configuration for the observer. Field defaults are set
// by Default(). Partial TOML files (including missing sections) are supported
// — unspecified fields retain their defaults.
type Config struct {
	Observer     ObserverConfig     `toml:"observer"`
	Proxy        ProxyConfig        `toml:"proxy"`
	Compression  CompressionConfig  `toml:"compression"`
	Intelligence IntelligenceConfig `toml:"intelligence"`
}

// ObserverConfig groups settings for the capture side of the system.
type ObserverConfig struct {
	DBPath    string          `toml:"db_path"`
	LogLevel  string          `toml:"log_level"`
	Watch     WatchConfig     `toml:"watch"`
	Freshness FreshnessConfig `toml:"freshness"`
	Secrets   SecretsConfig   `toml:"secrets"`
	Retention RetentionConfig `toml:"retention"`
	Hooks     HooksConfig     `toml:"hooks"`
}

// WatchConfig controls the file watcher daemon.
type WatchConfig struct {
	PollIntervalSeconds int      `toml:"poll_interval_seconds"`
	MaxFileSizeMB       int      `toml:"max_file_size_mb"`
	EnabledAdapters     []string `toml:"enabled_adapters"`
}

// FreshnessConfig controls content hashing and classification.
type FreshnessConfig struct {
	EnableContentHashing bool     `toml:"enable_content_hashing"`
	MaxHashFileSizeMB    int      `toml:"max_hash_file_size_mb"`
	FastPathStatOnly     bool     `toml:"fast_path_stat_only"`
	IgnorePatterns       []string `toml:"ignore_patterns"`
}

// SecretsConfig controls the scrubbing pipeline.
type SecretsConfig struct {
	EnableScrubbing bool     `toml:"enable_scrubbing"`
	ExtraPatterns   []string `toml:"extra_patterns"`
}

// RetentionConfig controls DB pruning.
type RetentionConfig struct {
	MaxAgeDays            int  `toml:"max_age_days"`
	MaxDBSizeMB           int  `toml:"max_db_size_mb"`
	PruneOnStartup        bool `toml:"prune_on_startup"`
	ObserverLogMaxAgeDays int  `toml:"observer_log_max_age_days"`
}

// HooksConfig controls hook runtime.
type HooksConfig struct {
	TimeoutMS int `toml:"timeout_ms"`
}

// ProxyConfig controls the API reverse proxy.
type ProxyConfig struct {
	Enabled           bool   `toml:"enabled"`
	Port              int    `toml:"port"`
	AnthropicUpstream string `toml:"anthropic_upstream"`
	OpenAIUpstream    string `toml:"openai_upstream"`
}

// CompressionConfig groups all four compression layers' toggles.
type CompressionConfig struct {
	CodeGraph    CodeGraphConfig    `toml:"code_graph"`
	Shell        ShellConfig        `toml:"shell"`
	Indexing     IndexingConfig     `toml:"indexing"`
	Conversation ConversationConfig `toml:"conversation"`
}

// CodeGraphConfig controls the codebase-memory-mcp companion.
type CodeGraphConfig struct {
	Enabled     bool `toml:"enabled"`
	AutoInstall bool `toml:"auto_install"`
	AutoIndex   bool `toml:"auto_index"`
}

// ShellConfig controls shell output filtering.
type ShellConfig struct {
	Enabled         bool     `toml:"enabled"`
	ExcludeCommands []string `toml:"exclude_commands"`
}

// IndexingConfig controls FTS5 tool output indexing.
type IndexingConfig struct {
	Enabled         bool `toml:"enabled"`
	MaxExcerptBytes int  `toml:"max_excerpt_bytes"`
	Embeddings      bool `toml:"embeddings"`
}

// ConversationConfig controls conversation-level compression.
type ConversationConfig struct {
	Enabled       bool     `toml:"enabled"`
	Mode          string   `toml:"mode"`
	TargetRatio   float64  `toml:"target_ratio"`
	PreserveLastN int      `toml:"preserve_last_n"`
	CompressTypes []string `toml:"compress_types"`
}

// IntelligenceConfig groups intelligence-layer settings.
//
// MonthlyBudgetUSD is the user's self-set spend cap for the calendar
// month — surfaced on the Analysis dashboard as a progress tile. Zero
// disables budget tracking. Stored in `intelligence.monthly_budget_usd`.
// The Settings page (PR 2 of the dashboard refresh) writes this from the
// UI; until then users can edit `config.toml` directly.
type IntelligenceConfig struct {
	CodeGraph        IntelligenceCodeGraphConfig `toml:"code_graph"`
	Pricing          PricingConfig               `toml:"pricing"`
	APIKeyEnv        string                      `toml:"api_key_env"`
	SummaryModel     string                      `toml:"summary_model"`
	MonthlyBudgetUSD float64                     `toml:"monthly_budget_usd"`
}

// IntelligenceCodeGraphConfig is a distinct sub-config (separate from the
// compression layer) because a user may enable intelligence queries against
// the code graph without enabling the companion binary.
type IntelligenceCodeGraphConfig struct {
	Enabled bool `toml:"enabled"`
}

// PricingConfig carries per-model input/output/cache pricing.
type PricingConfig struct {
	Models map[string]ModelPricing `toml:"models"`
}

// ModelPricing is per-million-token pricing for a single model. CacheCreation
// is optional — when zero, the cost engine defaults it to 1.25 × Input
// (Anthropic's published cache-write premium). CacheCreation1h is the
// 1-hour ephemeral tier rate; defaults to 2 × CacheCreation when zero.
//
// LongContextThreshold + LongContext* model providers that reprice an
// entire request when the prompt exceeds a token threshold (Anthropic
// Sonnet 4 / 4.5 at 200K, OpenAI gpt-5.4 / gpt-5.5 at 272K, Gemini
// 2.5 Pro / 3.1 Pro at 200K). Threshold zero disables the tier; each
// LongContext* rate falls back to its standard counterpart when zero.
type ModelPricing struct {
	Input           float64 `toml:"input"`
	Output          float64 `toml:"output"`
	CacheRead       float64 `toml:"cache_read"`
	CacheCreation   float64 `toml:"cache_creation"`
	CacheCreation1h float64 `toml:"cache_creation_1h"`

	LongContextThreshold       int64   `toml:"long_context_threshold"`
	LongContextInput           float64 `toml:"long_context_input"`
	LongContextOutput          float64 `toml:"long_context_output"`
	LongContextCacheRead       float64 `toml:"long_context_cache_read"`
	LongContextCacheCreation   float64 `toml:"long_context_cache_creation"`
	LongContextCacheCreation1h float64 `toml:"long_context_cache_creation_1h"`
}

// Default returns the baked-in defaults (spec §16.1).
func Default() Config {
	return Config{
		Observer: ObserverConfig{
			DBPath:   "~/.observer/observer.db",
			LogLevel: "info",
			Watch: WatchConfig{
				PollIntervalSeconds: 2,
				MaxFileSizeMB:       50,
				EnabledAdapters: []string{
					"claude-code", "codex", "cline", "roo-code", "cursor", "copilot", "opencode", "openclaw", "pi",
				},
			},
			Freshness: FreshnessConfig{
				EnableContentHashing: true,
				MaxHashFileSizeMB:    10,
				FastPathStatOnly:     true,
				IgnorePatterns: []string{
					"node_modules/", ".git/", "vendor/", "dist/", "build/",
					"target/", "__pycache__/",
					"*.exe", "*.bin", "*.wasm",
				},
			},
			Secrets: SecretsConfig{
				EnableScrubbing: true,
			},
			Retention: RetentionConfig{
				MaxAgeDays:            180,
				MaxDBSizeMB:           500,
				PruneOnStartup:        true,
				ObserverLogMaxAgeDays: 30,
			},
			Hooks: HooksConfig{
				TimeoutMS: 500,
			},
		},
		Proxy: ProxyConfig{
			Enabled:           true,
			Port:              8820,
			AnthropicUpstream: "https://api.anthropic.com",
			OpenAIUpstream:    "https://api.openai.com",
		},
		Compression: CompressionConfig{
			CodeGraph: CodeGraphConfig{
				Enabled:     true,
				AutoInstall: true,
				AutoIndex:   true,
			},
			Shell: ShellConfig{
				Enabled:         true,
				ExcludeCommands: []string{"curl", "playwright"},
			},
			Indexing: IndexingConfig{
				Enabled:         true,
				MaxExcerptBytes: 2048,
			},
			Conversation: ConversationConfig{
				Enabled:       false,
				Mode:          "token",
				TargetRatio:   0.85,
				PreserveLastN: 5,
				CompressTypes: []string{"json", "logs", "text"},
			},
		},
		Intelligence: IntelligenceConfig{
			CodeGraph: IntelligenceCodeGraphConfig{Enabled: true},
			Pricing:   PricingConfig{Models: map[string]ModelPricing{}},
		},
	}
}

// LoadOptions parameterizes Load.
type LoadOptions struct {
	// GlobalPath overrides the location of the user-global config. Defaults to
	// ~/.observer/config.toml.
	GlobalPath string
	// ProjectPath, when set, is a per-project .observer/config.toml that
	// overrides the global file.
	ProjectPath string
	// Env is the environment lookup function. Defaults to os.Getenv.
	Env func(string) string
}

// Load merges defaults ← global TOML ← project TOML ← environment overrides.
// Missing TOML files are not errors (defaults apply). Env variable form:
// OBSERVER_<SECTION>_<KEY> (uppercased, underscores). Nested sections are
// joined with additional underscores, e.g. OBSERVER_COMPRESSION_CONVERSATION_ENABLED.
// ResolveGlobalPath returns the global config file path Load would
// use given the same override. Lets callers (notably the Settings
// page's PUT /api/config/pricing handler) locate the file for
// save-back operations without reimplementing the resolution rule.
func ResolveGlobalPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".observer", "config.toml"), nil
}

func Load(opts LoadOptions) (Config, error) {
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	cfg := Default()

	globalPath := opts.GlobalPath
	if globalPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			globalPath = filepath.Join(home, ".observer", "config.toml")
		}
	}
	if err := mergeTOMLFile(&cfg, globalPath); err != nil {
		return Config{}, err
	}
	if opts.ProjectPath != "" {
		if err := mergeTOMLFile(&cfg, opts.ProjectPath); err != nil {
			return Config{}, err
		}
	}
	applyEnvOverrides(&cfg, opts.Env)

	cfg.Observer.DBPath = expandHome(cfg.Observer.DBPath)

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func mergeTOMLFile(cfg *Config, path string) error {
	if path == "" {
		return nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config.Load: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(body, cfg); err != nil {
		return fmt.Errorf("config.Load: parse %s: %w", path, err)
	}
	return nil
}

// Validate checks semantic constraints on cfg.
func Validate(cfg Config) error {
	if cfg.Observer.DBPath == "" {
		return errors.New("config: observer.db_path is required")
	}
	switch cfg.Observer.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: observer.log_level %q not in {debug, info, warn, error}", cfg.Observer.LogLevel)
	}
	if cfg.Observer.Watch.PollIntervalSeconds < 0 {
		return errors.New("config: observer.watch.poll_interval_seconds must be >= 0")
	}
	if cfg.Observer.Hooks.TimeoutMS <= 0 {
		return errors.New("config: observer.hooks.timeout_ms must be > 0")
	}
	if cfg.Proxy.Enabled && (cfg.Proxy.Port <= 0 || cfg.Proxy.Port > 65535) {
		return fmt.Errorf("config: proxy.port %d out of range", cfg.Proxy.Port)
	}
	if cfg.Compression.Conversation.Enabled {
		switch cfg.Compression.Conversation.Mode {
		case "token", "cache":
		default:
			return fmt.Errorf("config: compression.conversation.mode %q not in {token, cache}", cfg.Compression.Conversation.Mode)
		}
		if r := cfg.Compression.Conversation.TargetRatio; r <= 0 || r >= 1 {
			return fmt.Errorf("config: compression.conversation.target_ratio %.2f must be in (0, 1)", r)
		}
	}
	return nil
}

// HookTimeout returns the hook timeout as a time.Duration.
func (c HooksConfig) HookTimeout() time.Duration {
	return time.Duration(c.TimeoutMS) * time.Millisecond
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// applyEnvOverrides walks cfg via reflection and applies any matching
// OBSERVER_<...> environment variables. Supports string, int, float64,
// bool, and []string (comma-separated).
func applyEnvOverrides(cfg *Config, env func(string) string) {
	v := reflect.ValueOf(cfg).Elem()
	applyEnvToStruct(v, []string{"OBSERVER"}, env)
}

func applyEnvToStruct(v reflect.Value, prefix []string, env func(string) string) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" {
			tag = field.Name
		}
		// Split embedded options like "name,omitempty".
		tag = strings.SplitN(tag, ",", 2)[0]
		if tag == "-" {
			continue
		}
		envSegment := strings.ToUpper(strings.ReplaceAll(tag, ".", "_"))
		newPrefix := append(append([]string{}, prefix...), envSegment)
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct {
			applyEnvToStruct(fv, newPrefix, env)
			continue
		}
		key := strings.Join(newPrefix, "_")
		raw := env(key)
		if raw == "" {
			continue
		}
		setEnvValue(fv, raw)
	}
}

func setEnvValue(fv reflect.Value, raw string) {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Int, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			fv.SetInt(n)
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			fv.SetFloat(f)
		}
	case reflect.Bool:
		if b, err := strconv.ParseBool(raw); err == nil {
			fv.SetBool(b)
		}
	case reflect.Slice:
		if fv.Type().Elem().Kind() == reflect.String {
			parts := strings.Split(raw, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			fv.Set(reflect.ValueOf(parts))
		}
	default:
		// Unsupported types are ignored — add cases as needed.
	}
}
