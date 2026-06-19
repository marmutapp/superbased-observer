package guard

import (
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Conformance matrix (guard spec §6.5): the SINGLE data table mapping
// every adapter × channel onto enforcement capabilities. This is
// where client differences live — as DATA, never as code branches
// (§3.3); boundaries look their channel up here (or hard-code the
// same values locally with a conformance test pinning agreement).
// The dashboard Security page (G7) renders this so F2 — most
// adapters can only flag post-hoc — is visible, not hidden.
//
// Q4 discipline: a channel's CanBlock/CanAsk is recorded only where
// the client DOCUMENTS deny semantics (Claude Code PreToolUse
// permissionDecision; Cursor hook permission JSON). Channels without
// documented deny semantics are observe-only here even when they are
// technically pre-execution — never assume.

// ChannelWatcher is the post-hoc transcript/DB-tail channel every
// adapter has.
const ChannelWatcher = "watcher"

// ConformanceEntry is one adapter×channel row of the §6.5 matrix.
type ConformanceEntry struct {
	// Client is the models.ToolXxx adapter name.
	Client string
	// Channel names the capture surface: "watcher", or
	// "hook:<event>" for hook receivers.
	Channel string
	// Caps are the channel's enforcement capabilities.
	Caps policy.Capabilities
	// Notes documents coverage caveats and degradation behavior —
	// rendered verbatim on status surfaces.
	Notes string
}

// watcherEntry builds the post-hoc row every adapter gets.
func watcherEntry(client string) ConformanceEntry {
	return ConformanceEntry{
		Client:  client,
		Channel: ChannelWatcher,
		Caps:    policy.Capabilities{},
		Notes:   "post-hoc flagging only; sees results (file changes, outputs) hooks structurally miss",
	}
}

// ConformanceMatrix returns the full §6.5 table. Order: hook channels
// first (the enforcement surfaces), then every adapter's watcher row.
func ConformanceMatrix() []ConformanceEntry {
	entries := []ConformanceEntry{
		{
			Client:  models.ToolClaudeCode,
			Channel: "hook:PreToolUse",
			Caps:    policy.Capabilities{PreExecution: true, CanBlock: true, CanAsk: true},
			Notes:   "native ask via permissionDecision; payload carries cwd but no project root, so boundary rules defer to the watcher",
		},
		{
			Client:  models.ToolCursor,
			Channel: "hook:" + "beforeShellExecution",
			Caps:    policy.Capabilities{PreExecution: true, CanBlock: true, CanAsk: true},
			Notes:   "permission JSON allow|deny|ask; payload carries the workspace root, so boundary rules are active pre-execution",
		},
		{
			Client:  models.ToolCursor,
			Channel: "hook:" + "beforeMCPExecution",
			Caps:    policy.Capabilities{PreExecution: true, CanBlock: true, CanAsk: true},
			Notes:   "permission JSON allow|deny|ask",
		},
		{
			Client:  models.ToolCursor,
			Channel: "hook:" + "beforeReadFile",
			Caps:    policy.Capabilities{PreExecution: true, CanBlock: true, CanAsk: true},
			Notes:   "permission JSON allow|deny|ask",
		},
		{
			Client:  models.ToolCodex,
			Channel: "hook:notify",
			Caps:    policy.Capabilities{},
			Notes:   "observe-only notification channel — no documented deny semantics (Q4); enforcement comes from the native rules dialect once G11 compiles it",
		},
		{
			Client:  models.ToolHermes,
			Channel: "hook:plugin",
			Caps:    policy.Capabilities{},
			Notes:   "observe-only audit-log plugin bridge — no documented deny semantics (Q4)",
		},
	}
	// Every adapter has the watcher channel — including the
	// hook-capable ones (the hook sees declared calls; the watcher
	// sees results: the F1 mitigation).
	for _, client := range []string{
		models.ToolClaudeCode, models.ToolCodex, models.ToolCursor,
		models.ToolCline, models.ToolClineCLI, models.ToolRooCode,
		models.ToolCopilot, models.ToolCopilotCLI, models.ToolCowork,
		models.ToolOpenCode, models.ToolOpenClaw, models.ToolPi,
		models.ToolGeminiCLI, models.ToolAntigravity, models.ToolHermes,
		models.ToolKiloCode, models.ToolKiloCodeCLI,
	} {
		entries = append(entries, watcherEntry(client))
	}
	return entries
}

// CapabilitiesFor looks up one adapter×channel row. ok=false means
// the channel isn't in the matrix — callers treat unknown channels as
// observe-only (the zero Capabilities), never as blockable.
func CapabilitiesFor(client, channel string) (policy.Capabilities, bool) {
	for _, e := range ConformanceMatrix() {
		if e.Client == client && e.Channel == channel {
			return e.Caps, true
		}
	}
	return policy.Capabilities{}, false
}

// ClassifyActionType maps a models action-type onto the policy event
// vocabulary — the same boundary classification the ingest seam uses,
// exported so hook receivers that already produce normalized
// ToolEvents (cursor) classify identically. ok=false means the action
// is not an evaluable kind.
func ClassifyActionType(actionType string) (policy.EventKind, bool) {
	return classifyKind(actionType)
}
