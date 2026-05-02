package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// handleConfig serves GET /api/config — the full live config struct
// rendered to JSON. Settings UI uses this to populate every section's
// form (or read-only display); the Pricing edit path POSTs back via
// /api/config/pricing.
//
// The response includes the resolved config_path so the UI can show
// which file would be written on save and surface a clear "no path —
// running ephemeral" state.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"config_path": s.opts.ConfigPath,
		"config":      cfg,
		// Capabilities — every section the UI may edit. "pricing" hot-reloads
		// the cost engine in place (no restart). The rest write to disk and
		// require a daemon restart to take effect; the UI surfaces a
		// "Restart daemon" banner after each non-pricing save.
		"editable_sections": []string{
			"pricing", "observer", "watcher", "freshness", "retention",
			"hooks", "proxy", "compression", "intelligence",
		},
	})
}

// handleConfigPricing serves PUT /api/config/pricing — accepts the
// `intelligence.pricing.models` map and writes it back to config.toml.
// Reloads the cost engine in-place so Cost / Analysis / Session-detail
// surfaces pick up the new rates without a restart.
//
// Save flow:
//  1. Resolve config path (errors if not configured)
//  2. Load current config from disk
//  3. Replace cfg.Intelligence.Pricing.Models with the request body
//  4. Copy current config.toml → config.toml.bak (Option A — comments
//     lost on save, .bak preserves the prior version)
//  5. Marshal full struct to TOML, atomic temp-file rename
//  6. cost.Engine.Reload(cfg.Intelligence) — atomic.Pointer swap
//
// On any error before step 4, no files are touched. On error during 4–5,
// the .bak preserves the user's prior file.
func (s *Server) handleConfigPricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.ConfigPath == "" {
		http.Error(w, "config path not configured — server has no file to save to", http.StatusConflict)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Models map[string]config.ModelPricing `json:"models"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Models == nil {
		req.Models = map[string]config.ModelPricing{}
	}

	cfg, err := config.Load(config.LoadOptions{GlobalPath: s.opts.ConfigPath})
	if err != nil {
		writeErr(w, fmt.Errorf("load current config: %w", err))
		return
	}
	cfg.Intelligence.Pricing.Models = req.Models

	if err := writeConfigToml(s.opts.ConfigPath, cfg); err != nil {
		writeErr(w, err)
		return
	}

	if s.opts.CostEngine != nil {
		s.opts.CostEngine.Reload(cfg.Intelligence)
	}

	writeJSON(w, map[string]any{
		"saved":       true,
		"config_path": s.opts.ConfigPath,
		"backup_path": s.opts.ConfigPath + ".bak",
		"models":      cfg.Intelligence.Pricing.Models,
	})
}

// handleConfigPricingDefaults serves GET /api/config/pricing/defaults
// — the cost engine's baked-in pricing table as { model_id: Pricing }.
// Used by the Settings → Pricing form to render a defaults reference
// list and the "override this default" shortcut.
func (s *Server) handleConfigPricingDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"defaults": cost.BakedInDefaults(),
	})
}

// handleConfigSection serves PUT /api/config/section/<name> — the
// generic save path for every config section other than pricing
// (which has its own hot-reload-aware endpoint). Slice 2 of the
// Settings page wires this for: observer, watcher, freshness,
// retention, hooks, proxy, compression, intelligence.
//
// Save flow mirrors handleConfigPricing:
//  1. Resolve config path; require non-empty.
//  2. Load current config.
//  3. Replace the named section's fields with the request body.
//  4. writeConfigToml — backs up to .bak then atomic rename.
//  5. Response sets restart_required=true so the UI surfaces the
//     "Restart daemon" banner. Pricing's hot-reload doesn't apply
//     because the affected consumers (proxy listener, watcher
//     subscriptions, hook registrations, retention prune cycle, etc.)
//     bind config at startup.
func (s *Server) handleConfigSection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.ConfigPath == "" {
		http.Error(w, "config path not configured — server has no file to save to", http.StatusConflict)
		return
	}

	// Path is /api/config/section/<name>; strip the prefix.
	name := strings.TrimPrefix(r.URL.Path, "/api/config/section/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "section name required (e.g. /api/config/section/observer)", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, fmt.Errorf("load current config: %w", err))
		return
	}

	if err := applySectionUpdate(&cfg, name, body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := writeConfigToml(s.opts.ConfigPath, cfg); err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, map[string]any{
		"saved":            true,
		"section":          name,
		"config_path":      s.opts.ConfigPath,
		"backup_path":      s.opts.ConfigPath + ".bak",
		"restart_required": true,
	})
}

// applySectionUpdate decodes body as the named section's payload and
// writes it onto cfg. Section names map to either a top-level Config
// field, a nested ObserverConfig sub-struct, or a synthetic group of
// scalar Observer / Intelligence fields. Pricing is intentionally
// unhandled — that section has a dedicated endpoint with hot-reload.
func applySectionUpdate(cfg *config.Config, name string, body []byte) error {
	switch name {
	case "observer":
		// Top-level Observer scalars only. Nested sub-structs (Watch,
		// Freshness, Retention, Hooks) are exposed as their own
		// sections so saving "observer" doesn't clobber values the
		// user is editing in the watcher or retention pane.
		var sec struct {
			DBPath   string `json:"DBPath"`
			LogLevel string `json:"LogLevel"`
		}
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode observer section: %w", err)
		}
		cfg.Observer.DBPath = sec.DBPath
		cfg.Observer.LogLevel = sec.LogLevel
	case "watcher":
		var sec config.WatchConfig
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode watcher: %w", err)
		}
		cfg.Observer.Watch = sec
	case "freshness":
		var sec config.FreshnessConfig
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode freshness: %w", err)
		}
		cfg.Observer.Freshness = sec
	case "retention":
		var sec config.RetentionConfig
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode retention: %w", err)
		}
		cfg.Observer.Retention = sec
	case "hooks":
		var sec config.HooksConfig
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode hooks: %w", err)
		}
		cfg.Observer.Hooks = sec
	case "proxy":
		var sec config.ProxyConfig
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode proxy: %w", err)
		}
		cfg.Proxy = sec
	case "compression":
		var sec config.CompressionConfig
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode compression: %w", err)
		}
		cfg.Compression = sec
	case "intelligence":
		// Pricing has its own endpoint with hot-reload, so don't let
		// this save path clobber it. Decode the editable subset, then
		// restore Pricing from the prior cfg.
		var sec struct {
			CodeGraph        config.IntelligenceCodeGraphConfig `json:"CodeGraph"`
			APIKeyEnv        string                             `json:"APIKeyEnv"`
			SummaryModel     string                             `json:"SummaryModel"`
			MonthlyBudgetUSD float64                            `json:"MonthlyBudgetUSD"`
		}
		if err := json.Unmarshal(body, &sec); err != nil {
			return fmt.Errorf("decode intelligence: %w", err)
		}
		cfg.Intelligence.CodeGraph = sec.CodeGraph
		cfg.Intelligence.APIKeyEnv = sec.APIKeyEnv
		cfg.Intelligence.SummaryModel = sec.SummaryModel
		cfg.Intelligence.MonthlyBudgetUSD = sec.MonthlyBudgetUSD
		// Pricing intentionally untouched.
	case "pricing":
		return errors.New("pricing has its own endpoint /api/config/pricing")
	default:
		return fmt.Errorf("unknown section %q", name)
	}
	return nil
}

// handleAdminRestart serves POST /api/admin/restart — schedules an
// os.Exit(0) ~500ms after returning so the browser response lands
// before the process tears down. Whether the daemon comes back depends
// on the supervisor (npm wrapper, systemd, manual relaunch). The UI
// shows a "if you don't see the dashboard in 10s, relaunch manually"
// hint after firing this.
//
// No CSRF token: the dashboard binds to localhost-only by default and
// the project hasn't shipped a network-mode threat model. Add a
// per-session token if remote-mode lands later.
func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"restart_scheduled": true, "delay_ms": 500})
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.opts.Logger.Info("admin restart triggered — exiting")
		os.Exit(0)
	}()
}

// handleBackfillStatus serves GET /api/backfill/status — surfaces every
// `observer backfill --<mode>` flag with a candidate-row count where
// the candidate set is computable in pure SQL (is-sidechain, cache-tier,
// message-id). The file-walking modes (per-adapter scans against
// ~/.claude/projects, opencode.db, etc.) report `candidates: -1` and a
// note that a scan is needed — running the backfill itself is the
// only way to count there.
//
// PR 2 of the dashboard refresh ships read-only. A subsequent PR adds
// `POST /api/backfill/run` for in-dashboard kick-offs.
func (s *Server) handleBackfillStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	type modeStatus struct {
		Mode           string `json:"mode"`
		Flag           string `json:"flag"`
		Description    string `json:"description"`
		Candidates     int64  `json:"candidates"` // -1 = needs file scan
		CandidatesNote string `json:"candidates_note,omitempty"`
	}

	// SQL-checkable modes — count rows that haven't been touched by the
	// matching backfill. Approximate (a NULL column may be platform-truth
	// rather than missing data); the figures are advisory, not gates.
	sqlModes := []struct {
		mode, flag, description, query string
	}{
		{
			"is-sidechain", "--is-sidechain",
			"actions.is_sidechain from JSONL (Claude Code parent/sub-agent boundary)",
			`SELECT COUNT(*) FROM actions WHERE is_sidechain IS NULL`,
		},
		{
			"cache-tier", "--cache-tier",
			"api_turns.cache_creation_1h_tokens from JSONL (since migration 008)",
			`SELECT COUNT(*) FROM api_turns WHERE cache_creation_tokens > 0
			   AND (cache_creation_1h_tokens IS NULL OR cache_creation_1h_tokens = 0)`,
		},
		{
			"message-id", "--message-id",
			"actions + token_usage.message_id (claudecode + codex + cursor + opencode)",
			`SELECT
			   (SELECT COUNT(*) FROM actions      WHERE message_id IS NULL OR message_id = '')
			 + (SELECT COUNT(*) FROM token_usage WHERE message_id IS NULL OR message_id = '')`,
		},
	}
	out := make([]modeStatus, 0, len(sqlModes)+11)
	for _, m := range sqlModes {
		var n int64
		if err := s.opts.DB.QueryRowContext(r.Context(), m.query).Scan(&n); err != nil {
			s.opts.Logger.Warn("backfill status query", "mode", m.mode, "err", err)
			n = -1
		}
		out = append(out, modeStatus{
			Mode: m.mode, Flag: m.flag, Description: m.description, Candidates: n,
		})
	}

	// File-walking modes — count requires running a per-adapter scan
	// over source files (claudecode JSONL, opencode.db, etc.). Surface
	// the mode name and let the user kick the run from the CLI.
	fileWalk := []struct{ mode, flag, description string }{
		{"opencode-message-id", "--opencode-message-id", "opencode.db row IDs (assistant rows + parent message ids)"},
		{"opencode-parts", "--opencode-parts", "opencode tool output excerpts from State.Output / Metadata.Output"},
		{"opencode-tokens", "--opencode-tokens", "re-ingest opencode token_usage rows missed pre-fix"},
		{"openclaw-action-types", "--openclaw-action-types", "spawn_subagent action_type for sessions_spawn rows"},
		{"openclaw-model", "--openclaw-model", "sessions.model + workspace_dir from sessions.json aliases"},
		{"openclaw-reasoning", "--openclaw-reasoning", "preceding_reasoning from openclaw JSONL assistant text/thinking parts"},
		{"codex-reasoning", "--codex-reasoning", "codex preceding_reasoning from agent_message events"},
		{"cursor-model", "--cursor-model", "actions.model from cursor rawHookPayload.Model"},
		{"copilot-message-id", "--copilot-message-id", "actions.message_id from spanId / parentSpanId"},
		{"pi-message-id", "--pi-message-id", "actions.message_id from pi message ids"},
		{"claudecode-user-prompts", "--claudecode-user-prompts", "user_prompt action rows for Claude Code text user lines"},
		{"claudecode-api-errors", "--claudecode-api-errors", "api_error action rows for Claude Code system/api_error JSONL records (content-policy blocks, rate limits, invalid-request errors)"},
	}
	for _, m := range fileWalk {
		out = append(out, modeStatus{
			Mode: m.mode, Flag: m.flag, Description: m.description,
			Candidates:     -1,
			CandidatesNote: "file scan needed — run from CLI to find candidates",
		})
	}

	writeJSON(w, map[string]any{
		"modes": out,
	})
}

// loadConfigForDashboard wraps config.Load with a friendlier behaviour
// for the dashboard's read path: when the file doesn't exist yet,
// return defaults rather than erroring. The Settings UI shows defaults
// + a "config file not yet created" hint until the user saves something.
func loadConfigForDashboard(path string) (config.Config, error) {
	if path == "" {
		return config.Default(), nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return config.Default(), nil
	}
	return config.Load(config.LoadOptions{GlobalPath: path})
}

// writeConfigToml saves cfg to path with a .bak fallback (Option A from
// the planning doc — comments lost on save, prior version preserved).
//
// Steps:
//  1. If path exists, copy it to path+".bak" so the user can recover
//     hand-written comments.
//  2. Marshal cfg to TOML in a temp file in the same directory (atomic
//     rename requires same filesystem).
//  3. os.Rename to path. If this fails, the .bak from step 1 is the
//     authoritative backup.
func writeConfigToml(path string, cfg config.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", existing, 0o644); err != nil {
			return fmt.Errorf("write .bak: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read current config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if rename failed.
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(cfg); err != nil {
		tmp.Close()
		return fmt.Errorf("marshal toml: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
