package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter parses GitHub Copilot agent debug session logs written by VS Code
// under User/workspaceStorage/*/GitHub.copilot-chat/debug-logs/<session>/.
//
// Cache-token gap (audit C3): Copilot's debug-log llm_request span only
// surfaces inputTokens / outputTokens in its attrs. There is no cache_read
// or cache_creation field in the published shape — Copilot uses its own
// caching layer between the IDE and the upstream model and does not
// expose Anthropic-style ephemeral cache tier counts. As a result, cost
// rollups for Copilot will under-count any cached prompt-side tokens
// relative to what GitHub bills the user. The adapter writes 0 for the
// cache columns rather than estimate; if Copilot's debug shape ever adds
// cache fields, parseLine and rawUsage gain new struct tags then.
type Adapter struct {
	scrubber *scrub.Scrubber
	roots    []string
}

// New returns an adapter with platform defaults.
func New() *Adapter {
	return &Adapter{scrubber: scrub.New(), roots: defaultRoots()}
}

// NewWithOptions customizes scrubber and/or roots for tests.
func NewWithOptions(s *scrub.Scrubber, roots []string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	return &Adapter{scrubber: s, roots: roots}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolCopilot }

// WatchPaths implements adapter.Adapter.
func (a *Adapter) WatchPaths() []string { return a.roots }

// IsSessionFile implements adapter.Adapter.
//
// Copilot session files come in two formats:
//
//  1. Legacy debug-log: <ws>/GitHub.copilot-chat/debug-logs/<sess>/main.jsonl
//     (gated by github.copilot.chat.advanced.debug; v1.4.26 auto-flips it).
//  2. Modern snapshot+patches: <ws>/chatSessions/<sessId>.jsonl and
//     <globalStorage>/emptyWindowChatSessions/<sessId>.jsonl, both written
//     unconditionally by VS Code Copilot Chat ≥0.45.
//
// Paths originate on Windows and macOS; we normalize both native and foreign
// separators so the matcher works regardless of host OS (Linux CI sees
// Windows-formatted paths in fixtures and tests).
func (*Adapter) IsSessionFile(path string) bool {
	return isLegacySessionPath(path) || isModernSessionPath(path)
}

func isLegacySessionPath(path string) bool {
	lower := strings.ReplaceAll(strings.ToLower(path), `\`, "/")
	return strings.HasSuffix(lower, "/main.jsonl") &&
		strings.Contains(lower, "/github.copilot-chat/debug-logs/")
}

func isModernSessionPath(path string) bool {
	lower := strings.ReplaceAll(strings.ToLower(path), `\`, "/")
	if !strings.HasSuffix(lower, ".jsonl") {
		return false
	}
	return strings.Contains(lower, "/chatsessions/") ||
		strings.Contains(lower, "/emptywindowchatsessions/")
}

type rawLine struct {
	Version      int             `json:"v"`
	TimestampMS  int64           `json:"ts"`
	DurationMS   int64           `json:"dur"`
	SessionID    string          `json:"sid"`
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	SpanID       string          `json:"spanId"`
	ParentSpanID string          `json:"parentSpanId"`
	Status       string          `json:"status"`
	Attrs        json.RawMessage `json:"attrs"`
}

type commonAttrs struct {
	Content      string `json:"content"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	TTFT         int64  `json:"ttft"`
	UserRequest  string `json:"userRequest"`
	Args         string `json:"args"`
	Result       string `json:"result"`
	Response     string `json:"response"`
	Reasoning    string `json:"reasoning"`
}

type toolArgs struct {
	Path     string `json:"path"`
	File     string `json:"file"`
	FilePath string `json:"filePath"`
	Command  string `json:"command"`
	Cmd      string `json:"cmd"`
	Query    string `json:"query"`
	Pattern  string `json:"pattern"`
	URL      string `json:"url"`
}

// ParseSessionFile implements adapter.Adapter. It dispatches between the
// legacy debug-log scanner and the modern snapshot+patches parser based on
// the path shape (see isLegacySessionPath / isModernSessionPath).
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	return a.dispatchParse(ctx, path, fromOffset)
}

type sessionContext struct {
	SessionID   string
	ProjectRoot string
	Model       string
}

func (a *Adapter) parseLine(sourceFile string, line rawLine, lineNum int, state *sessionContext, res *adapter.ParseResult) {
	if line.SessionID != "" {
		state.SessionID = line.SessionID
	}
	ts := millisToTime(line.TimestampMS)

	var attrs commonAttrs
	if len(line.Attrs) > 0 {
		_ = json.Unmarshal(line.Attrs, &attrs)
	}

	switch line.Type {
	case "user_message":
		text := strings.TrimSpace(attrs.Content)
		if text == "" {
			return
		}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile:         sourceFile,
			SourceEventID:      firstNonEmpty(line.SpanID, fmt.Sprintf("user:L%d", lineNum)),
			SessionID:          state.SessionID,
			MessageID:          "user:" + firstNonEmpty(line.SpanID, fmt.Sprintf("user:L%d", lineNum)),
			ProjectRoot:        state.ProjectRoot,
			Timestamp:          ts,
			Model:              state.Model,
			Tool:               models.ToolCopilot,
			ActionType:         models.ActionUserPrompt,
			Target:             truncate(text, 200),
			Success:            true,
			PrecedingReasoning: truncate(text, 200),
			RawToolName:        "user_message",
			RawToolInput:       a.scrubber.String(text),
		})
	case "tool_call":
		ev := a.toolCallEvent(sourceFile, line, lineNum, ts, *state, attrs)
		res.ToolEvents = append(res.ToolEvents, ev)
	case "llm_request":
		if attrs.Model != "" {
			state.Model = attrs.Model
		}
		if attrs.InputTokens != 0 || attrs.OutputTokens != 0 {
			res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
				SourceFile:    sourceFile,
				SourceEventID: firstNonEmpty(line.SpanID, fmt.Sprintf("usage:L%d", lineNum)),
				SessionID:     state.SessionID,
				MessageID:     assistantMessageID(line),
				ProjectRoot:   state.ProjectRoot,
				Timestamp:     ts,
				Tool:          models.ToolCopilot,
				Model:         state.Model,
				InputTokens:   attrs.InputTokens,
				OutputTokens:  attrs.OutputTokens,
				Source:        models.TokenSourceJSONL,
				Reliability:   models.ReliabilityApproximate,
			})
		}
	case "agent_response":
		output := extractAssistantText(attrs.Response)
		if output == "" {
			output = strings.TrimSpace(attrs.Response)
		}
		if output == "" && strings.TrimSpace(attrs.Reasoning) == "" {
			return
		}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile:         sourceFile,
			SourceEventID:      firstNonEmpty(line.SpanID, fmt.Sprintf("complete:L%d", lineNum)),
			SessionID:          state.SessionID,
			MessageID:          assistantMessageID(line),
			ProjectRoot:        state.ProjectRoot,
			Timestamp:          ts,
			Model:              state.Model,
			Tool:               models.ToolCopilot,
			ActionType:         models.ActionTaskComplete,
			Target:             "agent_response",
			Success:            strings.EqualFold(line.Status, "ok") || line.Status == "",
			DurationMs:         line.DurationMS,
			PrecedingReasoning: truncate(attrs.Reasoning, 200),
			RawToolName:        "agent_response",
			ToolOutput:         a.scrubber.String(output),
		})
	}
}

func (a *Adapter) toolCallEvent(sourceFile string, line rawLine, lineNum int, ts time.Time, state sessionContext, attrs commonAttrs) models.ToolEvent {
	target, rawInput := parseToolArgs(attrs.Args, line.Name)
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: firstNonEmpty(line.SpanID, fmt.Sprintf("tool:%s:L%d", line.Name, lineNum)),
		SessionID:     state.SessionID,
		MessageID:     assistantMessageID(line),
		ProjectRoot:   state.ProjectRoot,
		Timestamp:     ts,
		Model:         state.Model,
		Tool:          models.ToolCopilot,
		ActionType:    mapToolName(line.Name),
		Target:        truncate(target, 200),
		Success:       strings.EqualFold(line.Status, "ok") || line.Status == "",
		DurationMs:    line.DurationMS,
		RawToolName:   line.Name,
		RawToolInput:  a.scrubber.String(rawInput),
		ToolOutput:    a.scrubber.String(attrs.Result),
	}
}

func mapToolName(name string) string {
	// Modern Copilot agent tools arrive in camelCase ("runInTerminal");
	// legacy debug-log tools arrive in snake_case ("run_in_terminal").
	// Normalizing to lowercase-with-underscores-stripped collapses both
	// into a single key, so we don't have to maintain parallel entries.
	key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "")
	switch key {
	case "managetodolist":
		return models.ActionTodoUpdate
	case "runsubagent":
		return models.ActionSpawnSubagent
	case "readfile", "openfile", "readsemantic", "searchbyname", "viewimage":
		return models.ActionReadFile
	case "createfile", "writefile":
		return models.ActionWriteFile
	case "replacestringinfile", "replacelinesinfile", "applypatch", "deletefile", "editfiles":
		return models.ActionEditFile
	case "runinterminal", "executecommand", "shell":
		return models.ActionRunCommand
	case "findtextinfiles", "grep", "grepsearch":
		return models.ActionSearchText
	case "filesearch", "findfiles", "listdir":
		return models.ActionSearchFiles
	case "fetchwebpage", "webfetch":
		return models.ActionWebFetch
	case "websearch":
		return models.ActionWebSearch
	default:
		return models.ActionUnknown
	}
}

func parseToolArgs(raw string, fallback string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, raw
	}
	var args toolArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return fallback, raw
	}
	for _, v := range []string{args.Path, args.File, args.FilePath, args.Command, args.Cmd, args.Query, args.Pattern, args.URL} {
		if strings.TrimSpace(v) != "" {
			return v, raw
		}
	}
	return fallback, raw
}

func extractAssistantText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var payload []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"parts"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	var parts []string
	for _, msg := range payload {
		if msg.Role != "assistant" {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type == "text" && strings.TrimSpace(part.Content) != "" {
				parts = append(parts, part.Content)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// defaultRoots emits the VS Code User/workspaceStorage and User/globalStorage
// paths appropriate to each cross-mount-resolved $HOME's logical OS. Copilot
// Chat only runs inside VS Code, so the per-OS prefix is fixed:
//
//   - windows: $HOME\AppData\Roaming\Code\User\{workspaceStorage,globalStorage}
//   - darwin:  $HOME/Library/Application Support/Code/User/{workspaceStorage,globalStorage}
//   - linux:   $HOME/.config/Code/User/{workspaceStorage,globalStorage}
//
// workspaceStorage hosts both the legacy debug-logs path and the modern
// chatSessions path. globalStorage hosts the modern emptyWindowChatSessions
// path (chats opened with no folder attached). Branching on h.OS (logical)
// instead of runtime.GOOS (host) is the fix for the WSL2-installed observer
// never seeing Copilot data living at /mnt/c/Users/<u>/AppData/...
func defaultRoots() []string {
	var roots []string
	for _, h := range crossmount.AllHomes() {
		var userDir string
		switch h.OS {
		case crossmount.OSWindows:
			userDir = filepath.Join(h.Path, "AppData", "Roaming", "Code", "User")
		case crossmount.OSDarwin:
			userDir = filepath.Join(h.Path, "Library", "Application Support", "Code", "User")
		case crossmount.OSLinux:
			userDir = filepath.Join(h.Path, ".config", "Code", "User")
		default:
			continue
		}
		roots = append(roots,
			filepath.Join(userDir, "workspaceStorage"),
			filepath.Join(userDir, "globalStorage", "emptyWindowChatSessions"),
		)
	}
	return roots
}

// sessionIDFromPath returns the session identifier embedded in a Copilot
// session file path. Legacy debug-logs put the id in the parent directory
// (`<sess>/main.jsonl`); modern chatSessions and emptyWindowChatSessions
// put it in the file basename (`<sess>.jsonl`).
func sessionIDFromPath(path string) string {
	if isModernSessionPath(path) {
		return strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return filepath.Base(filepath.Dir(path))
}

// projectRootFromPath walks up from the source file's directory until it
// finds an ancestor whose parent is named `workspaceStorage` — that
// ancestor is the workspace ID dir. We use filepath.Dir for the walk
// because the previous Split + filepath.Join approach silently dropped
// the leading separator on absolute Linux/macOS paths (filepath.Join
// strips empty leading elements), turning `/tmp/.../ws-1` into the
// relative `tmp/.../ws-1` and breaking the workspace.json read.
//
// Returns the resolved folder URI from `workspace.json` when present,
// the workspace ID dir as a fallback, or `[copilot]` if the source
// file isn't under a workspaceStorage tree at all.
func projectRootFromPath(path string) string {
	for cur := filepath.Dir(path); cur != "" && cur != filepath.Dir(cur); cur = filepath.Dir(cur) {
		if filepath.Base(filepath.Dir(cur)) != "workspaceStorage" {
			continue
		}
		if root := workspaceFolderFromMetadata(cur); root != "" {
			return root
		}
		return cur
	}
	return "[copilot]"
}

func assistantMessageID(line rawLine) string {
	root := firstNonEmpty(line.ParentSpanID, line.SpanID)
	if root == "" {
		return ""
	}
	return "assistant:" + root
}

func workspaceFolderFromMetadata(workspaceStorageDir string) string {
	body, err := os.ReadFile(filepath.Join(workspaceStorageDir, "workspace.json"))
	if err != nil {
		return ""
	}
	var payload struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return decodeFileURI(payload.Folder)
}

var windowsDrivePathRE = regexp.MustCompile(`^/[A-Za-z]:/`)

func decodeFileURI(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !strings.EqualFold(u.Scheme, "file") {
		return ""
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return ""
	}
	if windowsDrivePathRE.MatchString(p) {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

func millisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
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
