package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/marmutapp/superbased-observer/internal/launch"
)

// Launch endpoint (usability arc P4.6 / review row L2b): POST
// /api/tools/launch opens a terminal running the requested tool,
// best-effort, and ALWAYS returns the copy-paste command — when the
// host has no spawn mechanism (headless, no interop) or the spawn
// fails, spawned=false and the command is the product. Never fakes
// success. The tool name is the only user input and is validated
// against launch's hardcoded allow-list before anything is built.

// launchDetect / launchSpawn are the launch package seams, package
// vars so tests never spawn a real terminal window.
var (
	launchDetect = launch.Detect
	launchSpawn  = launch.Spawn
)

// handleToolsLaunch serves POST /api/tools/launch {"tool"}.
func (s *Server) handleToolsLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Tool string `json:"tool"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Routed tools get a plain launch (the tool's own config reaches
	// the proxy; native OAuth refresh stays in charge — D13); unrouted
	// tools get the observer wrapper.
	routed := false
	switch req.Tool {
	case "claude-code":
		routed = s.claudeSetupSnapshot().RoutedToObserver
	case "codex":
		routed = s.codexSetupSnapshot().Status == "routed_to_observer"
	}

	spec, err := launch.Plan(launch.Request{Tool: req.Tool, Routed: routed}, launchDetect())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := map[string]any{
		"tool":    req.Tool,
		"routed":  routed,
		"command": spec.Command,
		"method":  spec.Method,
		"spawned": false,
	}
	switch {
	case len(spec.Argv) == 0:
		resp["detail"] = spec.Reason
	default:
		if spawnErr := launchSpawn(r.Context(), spec); spawnErr != nil {
			resp["detail"] = spawnErr.Error()
		} else {
			resp["spawned"] = true
		}
	}
	writeJSON(w, resp)
}
