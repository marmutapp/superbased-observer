// Package freebuff implements an adapter for Freebuff, the free CodebuffAI
// coding agent (npm `freebuff`; the Manicode -> Codebuff -> Freebuff
// lineage, which is why the store lives under a legacy `manicode` dir).
//
// On-disk layout (same on Linux, macOS, and Windows — Freebuff uses
// ~/.config/manicode on every OS, per the CodebuffAI/freebuff Windows
// bug report referencing .config\manicode\freebuff.exe):
//
//	~/.config/manicode/projects/<slug>/chats/<RFC3339-timestamp>/
//	  chat-messages.json  the transcript: an array of message objects,
//	                      variant "user"|"ai", each with a `blocks` array
//	                      (text/tool/agent/mode-divider). The <RFC3339>
//	                      dir name is the `freebuff --continue <id>` handle.
//	  run-state.json      large state sidecar; sessionState.fileContext.
//	                      projectRoot is the ONLY statement of the real cwd.
//
// THIN store: Freebuff records NO per-turn token accounting (run-state has a
// running contextTokenCount, which is a context-window size, not billable
// usage), so this adapter emits sessions + actions only — no TokenEvents.
//
// Off-limits (never read): credentials.json, the freebuff ELF/exe binary,
// message-history.json (raw input strings), and log.jsonl (which carries
// hostname / userId / userEmail PII) — this adapter reads only
// chat-messages.json + its sibling run-state.json. The avoidance is
// structural: IsSessionFile matches ONLY chat-messages.json under
// projects/<slug>/chats, and the sole sibling read is run-state.json.
// TestOffLimitsFilesNeverDispatchedOrIngested pins it against regression.
package freebuff

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

const (
	projectsSubpath = "/manicode/projects/"
	chatsSegment    = "/chats/"
	messagesName    = "chat-messages.json"
	runStateName    = "run-state.json"
	maxTargetLen    = 500
	maxReasoningLen = 2000
	// maxAgentNestDepth caps recursive descent into agent blocks' own
	// nested blocks arrays. Grounded real data nests one level deep (a
	// subagent's private tool-call transcript); the cap is a defensive
	// bound against adversarial/malformed input, not an observed depth.
	maxAgentNestDepth = 6
)

// Adapter parses Freebuff session directories.
type Adapter struct {
	scrubber *scrub.Scrubber
	roots    []string
}

// New returns an adapter with the default scrubber and platform-default roots.
func New() *Adapter {
	return &Adapter{scrubber: scrub.New(), roots: defaultRoots()}
}

// NewWithOptions customizes the scrubber and/or watch roots for tests.
func NewWithOptions(s *scrub.Scrubber, roots ...string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	return &Adapter{scrubber: s, roots: roots}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolFreebuff }

// WatchPaths implements adapter.Adapter.
func (a *Adapter) WatchPaths() []string { return a.roots }

// defaultRoots returns ~/.config/manicode/projects across every detected home
// (Freebuff uses .config/manicode on every OS).
func defaultRoots() []string {
	seen := map[string]bool{}
	var roots []string
	for _, h := range crossmount.AllHomes() {
		if h.Path == "" {
			continue
		}
		p := filepath.Join(h.Path, ".config", "manicode", "projects")
		if !seen[p] {
			seen[p] = true
			roots = append(roots, p)
		}
	}
	return roots
}

// IsSessionFile implements adapter.Adapter: the per-chat chat-messages.json
// under a watch root. run-state.json is read as a sibling, not tracked.
func (a *Adapter) IsSessionFile(path string) bool {
	if !matchesShape(path) {
		return false
	}
	return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
}

func matchesShape(path string) bool {
	lower := strings.ReplaceAll(strings.ToLower(path), `\`, "/")
	if filepath.Base(lower) != messagesName {
		return false
	}
	return strings.Contains(lower, projectsSubpath) && strings.Contains(lower, chatsSegment)
}

// freebuffMessage is one element of chat-messages.json.
type freebuffMessage struct {
	Variant string          `json:"variant"`
	Content string          `json:"content"`
	ID      string          `json:"id"`
	Blocks  []freebuffBlock `json:"blocks"`
}

type freebuffBlock struct {
	Type     string `json:"type"`
	TextType string `json:"textType"`
	Content  string `json:"content"`
	// tool block
	ToolName   string          `json:"toolName"`
	ToolCallID string          `json:"toolCallId"`
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	// agent block. InitialPrompt is empty in every real capture; the
	// actual invocation args live in Params (e.g. {"command":"..."}).
	// Blocks is the subagent's OWN private tool-call transcript — real
	// captures show it non-empty (walked recursively, depth-capped).
	AgentName     string          `json:"agentName"`
	AgentType     string          `json:"agentType"`
	InitialPrompt string          `json:"initialPrompt"`
	Params        json.RawMessage `json:"params"`
	Blocks        []freebuffBlock `json:"blocks"`
}

// ParseSessionFile implements adapter.Adapter. chat-messages.json is a whole
// JSON array rewritten in place, so the persisted cursor is a MESSAGE COUNT
// (fromOffset = messages already emitted), not a byte offset: it re-reads the
// file, emits messages[fromOffset-1:] (re-covering the last, possibly-updated
// message — block SourceEventIDs are stable so the store dedupes), and returns
// the new message count. No TokenEvents (Freebuff records no per-turn usage).
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	data, err := os.ReadFile(path) //nolint:gosec // watched session file
	if err != nil {
		return adapter.ParseResult{}, nil // vanished mid-poll; retry later
	}
	var msgs []freebuffMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		return adapter.ParseResult{NewOffset: fromOffset}, nil // partial write; retry
	}

	sessID := sessionIDFromPath(path)
	root, branch := a.resolveProjectRoot(path)
	base := parseChatDirTime(sessID)

	res := adapter.ParseResult{NewOffset: int64(len(msgs))}
	// Re-cover the last already-seen message (it may have grown new blocks).
	start := int(fromOffset) - 1
	if start < 0 {
		start = 0
	}
	for i := start; i < len(msgs); i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		a.emitMessage(&res, path, sessID, root, branch, ts, i, msgs[i])
	}
	return res, nil
}

func (a *Adapter) emitMessage(res *adapter.ParseResult, path, sessID, root, branch string, ts time.Time, msgIdx int, m freebuffMessage) {
	if m.Variant == "user" {
		text := a.scrubber.String(firstNonEmpty(m.Content, userTextFromBlocks(m.Blocks)))
		if text == "" {
			return
		}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile: path, SourceEventID: idFor(sessID, msgIdx, "0", "user"),
			SessionID: sessID, ProjectRoot: root, GitBranch: branch, Timestamp: ts,
			Tool: models.ToolFreebuff, ActionType: models.ActionUserPrompt,
			Target: truncate(text, maxTargetLen), Success: true,
		})
		return
	}
	// variant "ai": walk blocks (depth 0 = the message's own top-level array).
	a.emitBlocks(res, path, sessID, root, branch, ts, msgIdx, m.Blocks, "", 0)
}

// emitBlocks walks one blocks array (a message's top-level blocks, or an
// agent block's own nested transcript) and appends ToolEvents. idPrefix is
// the dotted SourceEventID path of the parent (empty at the top level, so a
// top-level block's id is unchanged from before this was made recursive);
// depth guards against unbounded recursion into nested agent blocks.
func (a *Adapter) emitBlocks(res *adapter.ParseResult, path, sessID, root, branch string, ts time.Time, msgIdx int, blocks []freebuffBlock, idPrefix string, depth int) {
	// sidechain is true for every block emitted from INSIDE a nested
	// agent block's own transcript (depth > 0) — i.e. everything this
	// call walks except the message's own top-level blocks (depth 0).
	// The "agent" block that SPAWNS a subagent is emitted at the depth
	// of its parent's context, so a top-level spawn (depth 0) is never
	// flagged, matching the convention that the spawn action itself is
	// not a sidechain — only the spawned work is. A nested agent block
	// found while already inside a subagent (depth > 0, agent-in-agent)
	// is itself sidechain, since spawning it is already subagent work.
	sidechain := depth > 0
	// A leading run of reasoning text threads onto the next actionable
	// block as PrecedingReasoning; scoped to this blocks array only — a
	// subagent's own transcript does not inherit its parent's reasoning.
	var reasoning string
	for bi, b := range blocks {
		blockPath := idPrefix + strconv.Itoa(bi)
		switch b.Type {
		case "text":
			if b.TextType == "reasoning" {
				reasoning = truncate(a.scrubber.String(firstNonEmpty(reasoning, b.Content)), maxReasoningLen)
				continue
			}
			if txt := a.scrubber.String(b.Content); txt != "" {
				res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
					SourceFile: path, SourceEventID: idFor(sessID, msgIdx, blockPath, "text"),
					SessionID: sessID, ProjectRoot: root, GitBranch: branch, Timestamp: ts,
					Tool: models.ToolFreebuff, ActionType: models.ActionAssistantMessage,
					Target: truncate(txt, maxTargetLen), Success: true, PrecedingReasoning: reasoning,
					IsSidechain: sidechain,
				})
				reasoning = ""
			}
		case "tool":
			action, target := mapFreebuffTool(b.ToolName, b.Input)
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile: path, SourceEventID: idFor(sessID, msgIdx, blockPath, firstNonEmpty(b.ToolCallID, "tool")),
				SessionID: sessID, ProjectRoot: root, GitBranch: branch, Timestamp: ts,
				Tool: models.ToolFreebuff, ActionType: action, RawToolName: b.ToolName,
				RawToolInput:       a.scrubber.RawJSON(b.Input),
				Target:             truncate(a.scrubber.String(target), maxTargetLen),
				ToolOutput:         a.scrubber.String(rawToText(b.Output)),
				Success:            true,
				PrecedingReasoning: reasoning,
				IsSidechain:        sidechain,
			})
			reasoning = ""
		case "agent":
			// InitialPrompt is empty in every real capture; Params carries the
			// actual invocation args (e.g. {"command":"..."}) and is the far
			// more useful target when present.
			target := firstNonEmpty(b.InitialPrompt, targetFromInput(b.Params), b.AgentName)
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile: path, SourceEventID: idFor(sessID, msgIdx, blockPath, "agent"),
				SessionID: sessID, ProjectRoot: root, GitBranch: branch, Timestamp: ts,
				Tool: models.ToolFreebuff, ActionType: models.ActionSpawnSubagent,
				RawToolName: firstNonEmpty(b.AgentType, b.AgentName, "agent"),
				Target:      truncate(a.scrubber.String(target), maxTargetLen),
				Success:     true, PrecedingReasoning: reasoning,
				IsSidechain: sidechain,
			})
			reasoning = ""
			// Real agent blocks carry their own nested tool-call transcript;
			// walk it (depth-capped) so a subagent's actions are captured too.
			if len(b.Blocks) > 0 && depth < maxAgentNestDepth {
				a.emitBlocks(res, path, sessID, root, branch, ts, msgIdx, b.Blocks, blockPath+".", depth+1)
			}
		default:
			// mode-divider and unknown block kinds are non-actionable.
		}
	}
}

// mapFreebuffTool maps a freebuff tool name to a normalized action + target.
func mapFreebuffTool(name string, input json.RawMessage) (string, string) {
	target := targetFromInput(input)
	switch name {
	case "read_files", "read_file":
		return models.ActionReadFile, target
	case "write_file", "create_file":
		return models.ActionWriteFile, target
	case "str_replace", "edit_file":
		return models.ActionEditFile, target
	case "run_terminal_command", "run_command", "bash":
		return models.ActionRunCommand, target
	case "code_search", "grep":
		return models.ActionSearchText, target
	case "find_files", "glob", "list_directory":
		return models.ActionSearchFiles, target
	case "web_search":
		return models.ActionWebSearch, target
	case "read_url", "web_fetch":
		return models.ActionWebFetch, target
	case "spawn_agents", "spawn_agent":
		// Defensive/likely-vestigial: real captures always represent a
		// subagent spawn as a "agent"-typed block (see emitBlocks), never
		// as a "tool"-typed block with this toolName — even though
		// spawn_agents does appear as an internal call in the app's own
		// debug log. Kept for forward compatibility.
		return models.ActionSpawnSubagent, target
	case "write_todos":
		return models.ActionTodoUpdate, target
	case "ask_user":
		return models.ActionAskUser, target
	case "skill":
		return models.ActionSkillInvoke, target
	case "browser_use":
		return models.ActionBrowserAction, target
	case "set_output":
		return models.ActionTaskComplete, target
	default:
		// tmux_cli, read_subtree, render_ui, gravity_index, file_picker,
		// context_pruner: present in the app's capability-list toolNames but
		// never observed as an actual invocation — left honestly unmapped
		// rather than guessed. See docs/freebuff-adapter.md known gaps.
		return models.ActionUnknown, target
	}
}

func targetFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "paths", "file_path", "filePath", "url", "query", "pattern"} {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func (a *Adapter) resolveProjectRoot(messagesPath string) (root, branch string) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(messagesPath), runStateName)) //nolint:gosec // sibling of a watched file
	if err != nil {
		return "[freebuff]", ""
	}
	var rs struct {
		SessionState struct {
			FileContext struct {
				ProjectRoot string `json:"projectRoot"`
				Cwd         string `json:"cwd"`
			} `json:"fileContext"`
		} `json:"sessionState"`
	}
	if err := json.Unmarshal(data, &rs); err != nil {
		return "[freebuff]", ""
	}
	cwd := strings.TrimSpace(firstNonEmpty(rs.SessionState.FileContext.ProjectRoot, rs.SessionState.FileContext.Cwd))
	if cwd == "" {
		return "[freebuff]", ""
	}
	cwd = crossmount.TranslateForeignPath(cwd)
	info, err := git.Resolve(cwd)
	if err != nil {
		return cwd, ""
	}
	return info.Root, info.Branch
}

// sessionIDFromPath returns the chat dir name — the RFC3339 timestamp that is
// the `freebuff --continue <id>` handle.
func sessionIDFromPath(messagesPath string) string {
	return filepath.Base(filepath.Dir(messagesPath))
}

// parseChatDirTime turns Freebuff's filesystem-safe chat dir name
// (2026-08-11T07-07-38.552Z, dashes where a timestamp has colons) into a
// time. Per-message timestamps in the transcript are display-only ("12:38
// PM", no date), so the dir name is the only real anchor.
func parseChatDirTime(dir string) time.Time {
	// Convert the two dashes in the time portion (after 'T') to colons.
	t := dir
	if i := strings.IndexByte(t, 'T'); i >= 0 {
		head, tail := t[:i+1], t[i+1:]
		tail = strings.Replace(tail, "-", ":", 2)
		t = head + tail
	}
	if v, err := time.Parse(time.RFC3339Nano, t); err == nil {
		return v
	}
	if v, err := time.Parse(time.RFC3339, t); err == nil {
		return v
	}
	return time.Time{}
}

func userTextFromBlocks(blocks []freebuffBlock) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Content != "" {
			return b.Content
		}
	}
	return ""
}

func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// idFor builds a stable per-block SourceEventID. blockPath is a dotted path
// (e.g. "4" for a top-level block, "4.1" for the second block inside the
// agent block at top-level index 4) so nested-agent blocks get distinct,
// stable ids without colliding with top-level ones.
func idFor(sessID string, msgIdx int, blockPath, kind string) string {
	return kind + ":" + sessID + ":" + strconv.Itoa(msgIdx) + ":" + blockPath
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
