package dashboard

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// Setup-wizard write endpoints (usability arc P4.2 / review row B3):
// the dashboard's per-step siblings of `observer init`'s hooks and
// MCP registration. Each POST performs EXACTLY ONE tool's one
// integration write, and only ever runs from an explicit user click
// behind the wizard's preview→confirm flow — the opt-in invariant (no
// AI-client config write without explicit per-action consent) holds
// per write, not per wizard run.
//
// Byte-equivalence with `observer init` is by construction: the same
// hook.Registry / mcp.Registrar drive both front doors, with the same
// --config rule (omitted when the daemon runs the default config
// path, passed when it runs a custom one).

// setupWizardHome, when non-empty, overrides the user home the
// registries write into. Tests set it so a wizard POST can never
// touch the developer's real ~/.claude / ~/.cursor / ~/.codex.
var setupWizardHome string

// wizardTools is the set of tools the hook + MCP registries manage.
var wizardTools = map[string]bool{
	"claude-code": true,
	"cursor":      true,
	"codex":       true,
}

type setupWizardRequest struct {
	Tool   string `json:"tool"`
	Force  bool   `json:"force"`
	DryRun bool   `json:"dry_run"`
}

// registrationConfigPath mirrors `observer init`'s --config rule: the
// registered command carries --config only when this daemon runs a
// non-default config file, so wizard writes stay byte-identical to a
// plain `observer init` on a default install.
func (s *Server) registrationConfigPath() string {
	if s.opts.ConfigPath == "" {
		return ""
	}
	if def, err := config.ResolveGlobalPath(""); err == nil && def == s.opts.ConfigPath {
		return ""
	}
	return s.opts.ConfigPath
}

func decodeWizardRequest(w http.ResponseWriter, r *http.Request) (setupWizardRequest, bool) {
	var req setupWizardRequest
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return req, false
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	if !wizardTools[req.Tool] {
		http.Error(w, "unknown tool (claude-code, cursor, codex)", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// handleSetupHooks serves POST /api/setup/hooks — register observer's
// hook entries into one tool's config file. {"tool", "force",
// "dry_run"}; conflicts come back 409 with the error + the partial
// result, force=true overrides (same contract as the routing POSTs).
func (s *Server) handleSetupHooks(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeWizardRequest(w, r)
	if !ok {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, err)
		return
	}
	reg, err := hook.NewRegistry(hook.Options{
		BinaryPath: exe,
		DryRun:     req.DryRun,
		Force:      req.Force,
		HomeDir:    setupWizardHome,
		ConfigPath: s.registrationConfigPath(),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	res := reg.Register(req.Tool)
	resp := map[string]any{
		"tool":        req.Tool,
		"config_path": res.ConfigPath,
		"hooks_added": res.HooksAdded,
		"already_set": res.AlreadySet,
		"dry_run":     res.DryRun,
	}
	if res.Error != nil {
		resp["error"] = res.Error.Error()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	writeJSON(w, resp)
}

// handleSetupMCP serves POST /api/setup/mcp — register the observer
// MCP server into one tool's config. Same request/conflict contract
// as handleSetupHooks. The wizard's MCP step carries the per-turn
// schema-overhead note and is never pre-selected — MCP stays opt-in
// (Default-On vs Opt-In; advisor E1 remediation routes through this
// consent flow only, per Q4).
func (s *Server) handleSetupMCP(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeWizardRequest(w, r)
	if !ok {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, err)
		return
	}
	reg, err := mcp.NewRegistrar(mcp.RegisterOptions{
		BinaryPath: exe,
		DryRun:     req.DryRun,
		Force:      req.Force,
		HomeDir:    setupWizardHome,
		ConfigPath: s.registrationConfigPath(),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	res := reg.Register(req.Tool)
	resp := map[string]any{
		"tool":        req.Tool,
		"config_path": res.ConfigPath,
		"added":       res.Added,
		"already_set": res.AlreadySet,
		"dry_run":     res.DryRun,
	}
	if res.Error != nil {
		resp["error"] = res.Error.Error()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	writeJSON(w, resp)
}
