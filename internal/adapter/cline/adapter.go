package cline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter parses Cline and Roo Code task files from VS Code globalStorage
// (spec §4.4). Both extensions use the same api_conversation_history.json
// format — essentially the Anthropic Messages content-block schema — so the
// same parser handles both.
//
// The owning tool (claude-code sense) is inferred from the path segment of
// the enclosing extension: saoudrizwan.claude-dev → cline,
// rooveterinaryinc.roo-cline → roo-code.
type Adapter struct {
	scrubber   *scrub.Scrubber
	watchRoots []string
}

// New returns an adapter with default scrubber and platform-specific watch
// paths.
func New() *Adapter {
	return &Adapter{scrubber: scrub.New()}
}

// NewWithOptions customizes the scrubber and/or watch roots. Non-empty
// watchRoots override platform defaults (useful for tests).
func NewWithOptions(s *scrub.Scrubber, watchRoots []string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	return &Adapter{scrubber: s, watchRoots: watchRoots}
}

// Name implements adapter.Adapter. Note: Cline and Roo Code share this
// adapter but the emitted Tool field on each ToolEvent is set per-file
// based on the enclosing extension directory.
func (*Adapter) Name() string { return models.ToolCline }

// WatchPaths returns the canonical Cline + Roo tasks directories under
// every cross-mount-resolved $HOME's VS Code globalStorage. The
// globalStorage subpath is per-OS (Windows uses %APPDATA%, etc.) and
// branches on h.OS so a WSL2 observer reaches Cline data living at
// /mnt/c/Users/<u>/AppData/Roaming/Code/User/globalStorage. Tests
// can override via NewWithOptions.
func (a *Adapter) WatchPaths() []string {
	if len(a.watchRoots) > 0 {
		return a.watchRoots
	}
	var roots []string
	for _, h := range crossmount.AllHomes() {
		base := vsCodeGlobalStorage(h)
		if base == "" {
			continue
		}
		roots = append(roots,
			filepath.Join(base, "saoudrizwan.claude-dev", "tasks"),
			filepath.Join(base, "rooveterinaryinc.roo-cline", "tasks"),
		)
	}
	return roots
}

// IsSessionFile matches api_conversation_history.json inside a tasks/<id>
// subdirectory.
func (*Adapter) IsSessionFile(path string) bool {
	return filepath.Base(path) == "api_conversation_history.json"
}

// actionMap translates Cline/Roo tool names to the normalized taxonomy.
var actionMap = map[string]string{
	"execute_command":       models.ActionRunCommand,
	"read_file":             models.ActionReadFile,
	"write_to_file":         models.ActionWriteFile,
	"replace_in_file":       models.ActionEditFile,
	"search_files":          models.ActionSearchText,
	"list_files":            models.ActionSearchFiles,
	"browser_action":        models.ActionBrowserAction,
	"attempt_completion":    models.ActionTaskComplete,
	"ask_followup_question": models.ActionAskUser,
}

type rawMessage struct {
	Role    string          `json:"role"`
	Ts      int64           `json:"ts"`
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage"`
	Model   string          `json:"model"`
}

type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ParseSessionFile implements adapter.Adapter.
//
// The Cline/Roo file is a JSON array, not JSONL, so we can't stream it
// line-by-line. Instead we parse the whole file and rely on store-level
// (source_file, source_event_id) idempotency to dedupe across re-parses.
// The returned NewOffset is the file size so the watcher can short-circuit
// subsequent calls when the file hasn't grown.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("cline.ParseSessionFile: stat: %w", err)
	}
	res := adapter.ParseResult{NewOffset: fi.Size()}
	if fromOffset > 0 && fromOffset >= fi.Size() {
		// File hasn't grown — nothing new. Skip full re-parse.
		return res, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("cline.ParseSessionFile: read: %w", err)
	}

	var msgs []rawMessage
	if err := json.Unmarshal(body, &msgs); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("malformed JSON: %v", err))
		return res, nil
	}

	toolID, sessionID := toolFromPath(path), sessionIDFromPath(path)
	projectRoot, gitBranch := a.inferProjectContext(path)
	pending := map[string]int{}

	for i := range msgs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		msg := &msgs[i]
		ts := parseMilliTimestamp(msg.Ts)

		if msg.Usage != nil {
			res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
				SourceFile:          path,
				SourceEventID:       fmt.Sprintf("tk:%s:%d", filepath.Base(filepath.Dir(path)), i),
				SessionID:           sessionID,
				ProjectRoot:         projectRoot,
				GitBranch:           gitBranch,
				Timestamp:           ts,
				Tool:                toolID,
				Model:               msg.Model,
				InputTokens:         msg.Usage.InputTokens,
				OutputTokens:        msg.Usage.OutputTokens,
				CacheReadTokens:     msg.Usage.CacheReadInputTokens,
				CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
				Source:              models.TokenSourceJSONL,
				Reliability:         models.ReliabilityApproximate,
			})
		}

		for _, block := range decodeContent(msg.Content) {
			switch block.Type {
			case "tool_use":
				evt := a.toolUseEvent(path, toolID, sessionID, projectRoot, gitBranch, msg.Model, ts, block)
				pending[block.ID] = len(res.ToolEvents)
				res.ToolEvents = append(res.ToolEvents, evt)
			case "tool_result":
				idx, ok := pending[block.ToolUseID]
				if !ok {
					continue
				}
				body := decodeResultContent(block.Content)
				scrubbed := a.scrubber.String(body)
				res.ToolEvents[idx].ToolOutput = scrubbed
				if block.IsError {
					res.ToolEvents[idx].Success = false
					res.ToolEvents[idx].ErrorMessage = truncate(scrubbed, 2048)
				}
				delete(pending, block.ToolUseID)
			}
		}
	}
	return res, nil
}

func (a *Adapter) toolUseEvent(
	sourceFile, toolID, sessionID, projectRoot, gitBranch, model string,
	ts time.Time,
	block rawContentBlock,
) models.ToolEvent {
	actionType, ok := actionMap[block.Name]
	if !ok {
		actionType = models.ActionUnknown
	}
	scrubbedInput := a.scrubber.RawJSON(block.Input)
	target := a.extractTarget(block.Name, block.Input, projectRoot)
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: block.ID,
		SessionID:     sessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     gitBranch,
		Model:         model,
		Tool:          toolID,
		ActionType:    actionType,
		Target:        target,
		Success:       true,
		RawToolName:   block.Name,
		RawToolInput:  firstNonEmpty(scrubbedInput, scrub.Truncate(string(block.Input))),
	}
}

func (a *Adapter) extractTarget(toolName string, rawInput json.RawMessage, projectRoot string) string {
	if len(rawInput) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(rawInput, &m); err != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch toolName {
	case "execute_command":
		return a.scrubber.String(pick("command"))
	case "read_file", "write_to_file", "replace_in_file":
		p := pick("path", "file_path")
		if p == "" {
			return ""
		}
		if projectRoot != "" {
			return git.RelativePath(projectRoot, p)
		}
		return p
	case "search_files":
		return pick("regex", "pattern")
	case "list_files":
		return pick("path")
	case "browser_action":
		return pick("action")
	case "ask_followup_question":
		return pick("question")
	case "attempt_completion":
		return pick("result")
	}
	return ""
}

// inferProjectContext walks up from the task file looking for a cwd hint.
// Cline/Roo don't record cwd in api_conversation_history.json directly, so
// we read a sibling ui_messages.json if present, or fall back to "" which
// leaves the project unresolved (the watcher still stores the action).
func (a *Adapter) inferProjectContext(path string) (projectRoot, branch string) {
	dir := filepath.Dir(path)
	ui := filepath.Join(dir, "ui_messages.json")
	body, err := os.ReadFile(ui)
	if err != nil {
		return "", ""
	}
	// ui_messages.json is an array of messages; the first often contains a
	// "say" block with type:"text" carrying the cwd in a task banner. A
	// cheap scan for the first "cwd" key is enough — we don't need the
	// full parse.
	var msgs []map[string]any
	if err := json.Unmarshal(body, &msgs); err != nil {
		return "", ""
	}
	for _, m := range msgs {
		if cwd, ok := m["cwd"].(string); ok && cwd != "" {
			info, err := git.Resolve(cwd)
			if err == nil {
				return info.Root, info.Branch
			}
			return cwd, ""
		}
	}
	return "", ""
}

// decodeContent handles the array-of-blocks form. Some Cline messages store
// content as a bare string (user-typed prompts) — we return a single text
// block in that case.
func decodeContent(raw json.RawMessage) []rawContentBlock {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var blocks []rawContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil
		}
		return blocks
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil
		}
		return []rawContentBlock{{Type: "text", Text: s}}
	}
	return nil
}

func decodeResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	if trimmed[0] == '[' {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		var b strings.Builder
		for i, bl := range blocks {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		}
		return b.String()
	}
	return ""
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		c := b[start]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			start++
			continue
		}
		break
	}
	for end > start {
		c := b[end-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			end--
			continue
		}
		break
	}
	return b[start:end]
}

// toolFromPath infers whether a task path belongs to Cline or Roo Code based
// on the enclosing extension directory.
func toolFromPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "rooveterinaryinc.roo-cline"):
		return models.ToolRooCode
	case strings.Contains(lower, "saoudrizwan.claude-dev"):
		return models.ToolCline
	}
	// Unrecognized extension — default to cline, which is the more common
	// of the two.
	return models.ToolCline
}

// sessionIDFromPath uses the task-directory name (a ULID-like string created
// by the extension) as the session ID.
func sessionIDFromPath(path string) string {
	return filepath.Base(filepath.Dir(path))
}

func parseMilliTimestamp(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// vsCodeGlobalStorage returns the VS Code globalStorage subpath under
// the given cross-mount-resolved $HOME, branching on the home's
// LOGICAL OS (not runtime.GOOS — those can differ when observer in
// WSL2 reaches a Windows /mnt/c/Users/<u> home).
//
// On windows the canonical location is %APPDATA%\Code\User\
// globalStorage. When h is the native windows home we honor APPDATA
// (handles roaming-profile redirection); for cross-mount Windows
// homes APPDATA is irrelevant — we use the conventional
// $HOME\AppData\Roaming layout, which is correct for the standard
// install. Returns "" for unrecognized OS tags.
func vsCodeGlobalStorage(h crossmount.HomeRoot) string {
	switch h.OS {
	case crossmount.OSWindows:
		if h.Origin == "native" && runtime.GOOS == "windows" {
			if appData := os.Getenv("APPDATA"); appData != "" {
				return filepath.Join(appData, "Code", "User", "globalStorage")
			}
		}
		return filepath.Join(h.Path, "AppData", "Roaming", "Code", "User", "globalStorage")
	case crossmount.OSDarwin:
		return filepath.Join(h.Path, "Library", "Application Support", "Code", "User", "globalStorage")
	case crossmount.OSLinux:
		return filepath.Join(h.Path, ".config", "Code", "User", "globalStorage")
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
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
