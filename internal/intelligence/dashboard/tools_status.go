package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// Connected-tools panel (usability arc P4.1 / review row B1): one
// status row per supported tool — detected / capturing / hooks / MCP /
// proxied — generalizing the per-tool /api/setup/* probes into a
// single matrix. Every probe is read-only (registry DryRun or plain
// file stat); the panel never writes AI-client config.

// toolProbe is one integration's read-only state for a tool. A nil
// probe on the row means the integration doesn't exist for that tool
// (honest n/a, not false).
type toolProbe struct {
	// Registered: the integration currently points at the observer.
	Registered bool `json:"registered"`
	// Partial: some but not all managed entries are registered
	// (hooks only — a subset of events point at the observer).
	Partial bool `json:"partial,omitempty"`
	// Detail is a one-line human explanation (counts, conflict text,
	// or the setup status word).
	Detail string `json:"detail,omitempty"`
}

// toolStatusRow is one tool's full status line.
type toolStatusRow struct {
	Tool         string     `json:"tool"`
	Detected     bool       `json:"detected"`
	DetectedPath string     `json:"detected_path,omitempty"`
	Enabled      bool       `json:"enabled"`
	ActionCount  int64      `json:"action_count"`
	LastSeenAt   string     `json:"last_seen_at,omitempty"`
	Hooks        *toolProbe `json:"hooks,omitempty"`
	MCP          *toolProbe `json:"mcp,omitempty"`
	Proxy        *toolProbe `json:"proxy,omitempty"`
}

// handleToolsStatus serves GET /api/tools/status — the Connected-tools
// matrix. Rows are the union of the injected adapter catalog and any
// tool with rows in the actions table (so foreign/legacy capture still
// shows). Sorted by tool name for a stable render.
func (s *Server) handleToolsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	type activity struct {
		lastSeen string
		count    int64
	}
	seen := map[string]activity{}
	// s.opts.DB, not s.db(): the connected-tools matrix describes THIS
	// machine's real capture activity even while demo mode is active.
	if s.opts.DB != nil {
		rows, err := s.opts.DB.QueryContext(r.Context(),
			`SELECT tool, MAX(timestamp), COUNT(*) FROM actions GROUP BY tool`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var tool, last string
				var n int64
				if rows.Scan(&tool, &last, &n) == nil {
					seen[tool] = activity{lastSeen: last, count: n}
				}
			}
			_ = rows.Err()
		}
	}

	enabled := map[string]bool{}
	if cfg, err := loadConfigForDashboard(s.opts.ConfigPath); err == nil {
		for _, name := range cfg.Observer.Watch.EnabledAdapters {
			enabled[name] = true
		}
	}

	out := make([]toolStatusRow, 0, len(s.opts.ToolCatalog)+4)
	covered := map[string]bool{}
	for _, entry := range s.opts.ToolCatalog {
		row := toolStatusRow{Tool: entry.Tool, Enabled: enabled[entry.Tool]}
		for _, p := range entry.WatchPaths {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				row.Detected = true
				row.DetectedPath = p
				break
			}
		}
		if a, ok := seen[entry.Tool]; ok {
			row.ActionCount = a.count
			row.LastSeenAt = a.lastSeen
		}
		s.attachIntegrationProbes(&row)
		covered[entry.Tool] = true
		out = append(out, row)
	}
	// Tools with DB activity but no catalog entry (legacy adapters,
	// foreign rows) still get a row — capture history never hides.
	for tool, a := range seen {
		if covered[tool] {
			continue
		}
		out = append(out, toolStatusRow{
			Tool:        tool,
			Enabled:     enabled[tool],
			ActionCount: a.count,
			LastSeenAt:  a.lastSeen,
		})
	}
	slices.SortFunc(out, func(a, b toolStatusRow) int {
		return strings.Compare(a.Tool, b.Tool)
	})

	writeJSON(w, map[string]any{
		"tools":        out,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// attachIntegrationProbes fills the hooks / MCP / proxy probes for
// tools that have those integrations. All probes are DryRun /
// read-only. Tools without an integration keep a nil probe (n/a).
func (s *Server) attachIntegrationProbes(row *toolStatusRow) {
	switch row.Tool {
	case "claude-code":
		row.Hooks = s.hookProbe("claude-code")
		row.MCP = s.mcpProbe("claude-code")
		snap := s.claudeSetupSnapshot()
		detail := snap.Status
		if snap.RoutedBaseURL != "" {
			detail = snap.RoutedBaseURL
		}
		row.Proxy = &toolProbe{Registered: snap.RoutedToObserver, Detail: detail}
	case "cursor":
		row.Hooks = s.hookProbe("cursor")
		row.MCP = s.mcpProbe("cursor")
	case "codex":
		row.Hooks = s.hookProbe("codex")
		row.MCP = s.mcpProbe("codex")
		snap := s.codexSetupSnapshot()
		row.Proxy = &toolProbe{
			Registered: snap.Status == "routed_to_observer",
			Detail:     snap.Status,
		}
	}
}

// hookProbe dry-runs the hook registry for one tool and folds the
// result into registered / partial / detail. The registry's
// AlreadySet detection is content-heuristic (the ` hook <tool> `
// token), so it recognizes registrations written by any observer
// binary — including the wsl.exe bridge shape.
func (s *Server) hookProbe(tool string) *toolProbe {
	exe, err := os.Executable()
	if err != nil {
		return &toolProbe{Detail: "probe failed: " + err.Error()}
	}
	reg, err := hook.NewRegistry(hook.Options{BinaryPath: exe, DryRun: true})
	if err != nil {
		return &toolProbe{Detail: "probe failed: " + err.Error()}
	}
	res := reg.Register(tool)
	if res.Error != nil {
		return &toolProbe{Detail: res.Error.Error()}
	}
	p := &toolProbe{
		Registered: len(res.AlreadySet) > 0 && len(res.HooksAdded) == 0,
		Partial:    len(res.AlreadySet) > 0 && len(res.HooksAdded) > 0,
	}
	switch {
	case p.Registered:
		p.Detail = fmt.Sprintf("%d events registered", len(res.AlreadySet))
	case p.Partial:
		p.Detail = fmt.Sprintf("%d registered, %d missing", len(res.AlreadySet), len(res.HooksAdded))
	default:
		p.Detail = fmt.Sprintf("would register %d events", len(res.HooksAdded))
	}
	return p
}

// mcpProbe dry-runs the MCP registrar for one tool. A conflict error
// (an `observer` entry pointing elsewhere) reads as registered=false
// with the conflict in detail — honest about stale registrations.
func (s *Server) mcpProbe(tool string) *toolProbe {
	exe, err := os.Executable()
	if err != nil {
		return &toolProbe{Detail: "probe failed: " + err.Error()}
	}
	reg, err := mcp.NewRegistrar(mcp.RegisterOptions{BinaryPath: exe, DryRun: true})
	if err != nil {
		return &toolProbe{Detail: "probe failed: " + err.Error()}
	}
	res := reg.Register(tool)
	if res.Error != nil {
		return &toolProbe{Detail: res.Error.Error()}
	}
	p := &toolProbe{Registered: res.AlreadySet}
	if res.AlreadySet {
		p.Detail = "observer MCP server registered"
	} else {
		p.Detail = "not registered"
	}
	return p
}
