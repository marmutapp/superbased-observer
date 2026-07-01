package cline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/platform/pathnorm"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// cwdEnvDetailsPattern matches the `Current Working Directory (<path>)`
// banner that the Cline VSCode extension (v3.88.0+) injects into the
// first user message's `<environment_details>` block. The path can
// contain backslashes / forward slashes / spaces but stops at the
// closing parenthesis or a newline. Replaces the older convention
// where ui_messages.json carried a top-level `cwd` key — gone as of
// v3.88.0.
var cwdEnvDetailsPattern = regexp.MustCompile(`Current Working Directory \(([^)\n]+)\)`)

// cwdScanBytes is the cap on how many bytes we read from
// api_conversation_history.json to find the env-details cwd banner.
// The banner lives in the first user message, well within the first
// 64 KiB even on long sessions. Bounds the cost of inferProjectContext
// on sessions with megabyte-scale histories.
const cwdScanBytes = 64 * 1024

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
		roots = append(
			roots,
			filepath.Join(base, "saoudrizwan.claude-dev", "tasks"),
			filepath.Join(base, "rooveterinaryinc.roo-cline", "tasks"),
		)
	}
	return roots
}

// IsSessionFile matches api_conversation_history.json inside one of
// this adapter's WatchPaths. The under-WatchPaths constraint enforces
// the v1.4.51 dispatch contract — basename-only predicates can't
// accidentally claim foreign files that happen to share the name.
func (a *Adapter) IsSessionFile(path string) bool {
	if filepath.Base(path) != "api_conversation_history.json" {
		return false
	}
	return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
}

// actionMap translates Cline/Roo tool names to the normalized taxonomy.
var actionMap = map[string]string{
	"execute_command":       models.ActionRunCommand,
	"powershell":            models.ActionRunCommand,
	"pwsh":                  models.ActionRunCommand,
	"cmd":                   models.ActionRunCommand,
	"cmd.exe":               models.ActionRunCommand,
	"bash":                  models.ActionRunCommand,
	"sh":                    models.ActionRunCommand,
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
	// Cline 3.89.2+ (non-Anthropic providers, e.g. providerId="cline")
	// dropped the Anthropic-shape `usage` block and instead carry
	// per-message `metrics` + `modelInfo` on assistant messages. See
	// tokenEventFor / resolvedModel.
	Metrics   *rawMetrics   `json:"metrics"`
	ModelInfo *rawModelInfo `json:"modelInfo"`
}

type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// rawMetrics is Cline 3.89.2's per-message token block (api_conversation_
// history.json assistant messages). Grounded 2026-06-26 against a live
// providerId="cline" session: tokens.prompt is NET input (it equals
// ui_messages api_req_started `tokensIn`, NOT a gross prompt total), cached
// equals `cacheReads`, completion equals `tokensOut`. Cache-WRITE tokens are
// NOT carried here (they live only in ui_messages
// api_req_started.text.cacheWrites), so this source emits CacheCreation=0.
// 2026-06-27 follow-up: the ordered cross-file join is validated (1:1,
// exact token correspondence), but cacheWrites was 0 across 14 live
// api_req_started requests (3 sessions) — the gap is a source
// characteristic, not a parse hole, so the sibling read is left unwired
// pending a real non-zero sample (see docs/cross-adapter-schema-mapping.md).
type rawMetrics struct {
	Tokens rawMetricTokens `json:"tokens"`
	Cost   float64         `json:"cost"`
}

type rawMetricTokens struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Cached     int64 `json:"cached"`
}

func (m rawMetricTokens) nonzero() bool {
	return m.Prompt != 0 || m.Completion != 0 || m.Cached != 0
}

// rawModelInfo is Cline 3.89.2's per-message model descriptor; the top-level
// `model` key is gone, so modelInfo.modelId is the model of record.
type rawModelInfo struct {
	ModelID string `json:"modelId"`
}

// resolvedModel returns the message's model, preferring the legacy top-level
// `model` key and falling back to Cline 3.89.2's modelInfo.modelId. Used for
// token rows AND tool/text rows so the model flows everywhere.
func (m *rawMessage) resolvedModel() string {
	if m.Model != "" {
		return m.Model
	}
	if m.ModelInfo != nil {
		return m.ModelInfo.ModelID
	}
	return ""
}

// tokenEventFor builds the per-message token event from whichever token
// shape the message carries: the Anthropic-shape `usage` (original Cline /
// Anthropic providers) OR Cline 3.89.2's `metrics` (non-Anthropic
// providers). Returns ok=false when neither is present (a turn that emitted
// no token accounting). model is the already-resolved model string.
func tokenEventFor(msg *rawMessage, path, sessionID, projectRoot, gitBranch, toolID, model string, ts time.Time, idx int) (models.TokenEvent, bool) {
	ev := models.TokenEvent{
		SourceFile:    path,
		SourceEventID: fmt.Sprintf("tk:%s:%d", filepath.Base(filepath.Dir(path)), idx),
		SessionID:     sessionID,
		ProjectRoot:   projectRoot,
		GitBranch:     gitBranch,
		Timestamp:     ts,
		Tool:          toolID,
		Model:         model,
		Source:        models.TokenSourceJSONL,
		Reliability:   models.ReliabilityApproximate,
	}
	switch {
	case msg.Usage != nil:
		ev.InputTokens = msg.Usage.InputTokens
		ev.OutputTokens = msg.Usage.OutputTokens
		ev.CacheReadTokens = msg.Usage.CacheReadInputTokens
		ev.CacheCreationTokens = msg.Usage.CacheCreationInputTokens
		return ev, true
	case msg.Metrics != nil && msg.Metrics.Tokens.nonzero():
		// prompt is NET (verified == ui_messages tokensIn), so it maps
		// straight to InputTokens with no gross-vs-cached netting.
		ev.InputTokens = msg.Metrics.Tokens.Prompt
		ev.OutputTokens = msg.Metrics.Tokens.Completion
		ev.CacheReadTokens = msg.Metrics.Tokens.Cached
		return ev, true
	default:
		return models.TokenEvent{}, false
	}
}

type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
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
		model := msg.resolvedModel()

		if ev, ok := tokenEventFor(msg, path, sessionID, projectRoot, gitBranch, toolID, model, ts, i); ok {
			res.TokenEvents = append(res.TokenEvents, ev)
		}

		blocks := decodeContent(msg.Content)

		// Surface the genuine user prompt as a user_prompt row (mirrors every
		// other adapter's per-message timeline). Cline wraps real user input
		// in <task>/<feedback>; tool-result and error-retry user messages are
		// skipped by extractUserPrompt so they don't masquerade as prompts.
		if msg.Role == "user" {
			if prompt := extractUserPrompt(blocks); prompt != "" {
				res.ToolEvents = append(res.ToolEvents, a.userPromptEvent(path, toolID, sessionID, projectRoot, gitBranch, model, ts, i, prompt))
			}
		}

		// reasoning accumulates the assistant's thinking blocks within this
		// message so the tool call / text that follows inherits it as
		// PrecedingReasoning (blocks are ordered: thinking precedes tool_use).
		var reasoning string
		for blockIdx, block := range blocks {
			switch block.Type {
			case "thinking", "redacted_thinking":
				if msg.Role != "assistant" {
					continue
				}
				txt := strings.TrimSpace(block.Thinking)
				if txt == "" && block.Type == "redacted_thinking" {
					txt = "[redacted thinking]"
				}
				if txt == "" {
					continue
				}
				reasoning = appendReasoning(reasoning, txt)
				// Emit the reasoning as a first-class, visible timeline row
				// (matches kilocode/opencode/codex), not only as downstream
				// PrecedingReasoning.
				res.ToolEvents = append(res.ToolEvents, a.reasoningEvent(path, toolID, sessionID, projectRoot, gitBranch, model, ts, i, blockIdx, txt))
			case "tool_use":
				evt := a.toolUseEvent(path, toolID, sessionID, projectRoot, gitBranch, model, ts, block)
				if reasoning != "" {
					evt.PrecedingReasoning = truncate(a.scrubber.String(reasoning), 2048)
				}
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
			case "text":
				if msg.Role != "assistant" {
					continue
				}
				body := strings.TrimSpace(block.Text)
				if body == "" {
					continue
				}
				res.ToolEvents = append(res.ToolEvents, a.assistantTextEvent(path, toolID, sessionID, projectRoot, gitBranch, model, ts, i, blockIdx, body))
			case "image":
				// Multimodal attachment (Anthropic image content block:
				// {type:"image", source:{type:"base64", media_type, data}}).
				// Cline stores pasted/attached images here; without a case
				// they fell through silently. Emit a marker row (image-only
				// user turns carry no <task> text, so extractUserPrompt
				// skips them) — observability-only, no image bytes stored.
				// The image's token cost lands on the per-message TokenEvent.
				res.ToolEvents = append(res.ToolEvents, a.imageEvent(path, toolID, sessionID, projectRoot, gitBranch, model, ts, i, blockIdx))
			}
		}
	}
	return res, nil
}

// assistantTextEvent emits a standalone assistant-text row for each text
// content block on a `role=assistant` message in the cline/roo conversation
// history. The file is re-read on every poll, so SourceEventID must be
// content-derivable for the store-layer (source_file, source_event_id)
// upsert to dedupe across re-parses — we use message-index + block-index
// + content-hash. No token/cost fields are set — observability-only,
// pricing is attributed via the existing per-message TokenEvent path.
// RawToolName uses the resolved toolID (cline / roo-code), matching the
// `<source>.assistant_text` convention.
func (a *Adapter) assistantTextEvent(
	sourceFile, toolID, sessionID, projectRoot, gitBranch, model string,
	ts time.Time,
	msgIdx, blockIdx int,
	body string,
) models.ToolEvent {
	preview := truncate(a.scrubber.String(body), 200)
	hash := shortHash(body)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("%s:asst:%s:%d:%d:%s", toolID, sessionID, msgIdx, blockIdx, hash),
		SessionID:          sessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          gitBranch,
		Model:              model,
		Tool:               toolID,
		ActionType:         models.ActionAssistantMessage,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        toolID + ".assistant_text",
		ToolOutput:         a.scrubber.String(contentcap.Cap(body, contentcap.DefaultMaxBytes)),
		MessageID:          toolID + ":asst:" + hash,
	}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// userPromptTagRE matches the wrappers Cline puts around genuine user input:
// <task> (initial prompt), <feedback> (reply to attempt_completion), and
// <user_message> (plan-mode reply). The (?s) flag lets the body span lines.
var userPromptTagRE = regexp.MustCompile(`(?s)<(?:task|feedback|user_message)>(.*?)</(?:task|feedback|user_message)>`)

// extractUserPrompt returns the genuine user-authored text from a role=user
// message's content blocks, or "" when the message is a programmatic
// continuation (a `[tool] Result:…` tool result, an `[ERROR] …` retry nudge,
// or an <environment_details> block) rather than a real prompt. Cline wraps
// real user input in <task>/<feedback>/<user_message>; everything else under
// role=user is auto-generated and must NOT surface as a user_prompt row.
// Grounded 2026-06-26 across live sessions (1782476071196 / 1782474409049 /
// 1782468429408).
func extractUserPrompt(blocks []rawContentBlock) string {
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if m := userPromptTagRE.FindStringSubmatch(b.Text); m != nil {
			if s := strings.TrimSpace(m[1]); s != "" {
				return s
			}
		}
	}
	return ""
}

// appendReasoning concatenates thinking segments within one assistant
// message, space-separated.
func appendReasoning(acc, next string) string {
	if acc == "" {
		return next
	}
	return acc + " " + next
}

// imageEvent emits a standalone marker row for an Anthropic image
// content block (a multimodal attachment in the cline/roo conversation
// history). The image bytes are never read or stored — only a
// "[image attachment]" marker so the multimodal activity is visible in
// the timeline. SourceEventID is derived from the message + block index
// so the (source_file, source_event_id) upsert dedupes across re-parses.
func (a *Adapter) imageEvent(
	sourceFile, toolID, sessionID, projectRoot, gitBranch, model string,
	ts time.Time,
	msgIdx, blockIdx int,
) models.ToolEvent {
	const marker = "[image attachment]"
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("%s:image:%s:%d:%d", toolID, sessionID, msgIdx, blockIdx),
		SessionID:     sessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     gitBranch,
		Model:         model,
		Tool:          toolID,
		ActionType:    models.ActionUserPrompt,
		Target:        marker,
		Success:       true,
		RawToolName:   toolID + ".image",
		MessageID:     fmt.Sprintf("%s:image:%s:%d:%d", toolID, sessionID, msgIdx, blockIdx),
	}
}

// reasoningEvent emits a standalone reasoning row for a thinking /
// redacted_thinking block on an assistant message, making the model's
// chain-of-thought visible in the action timeline (matches kilocode /
// opencode / codex). Content-derivable SourceEventID dedupes re-parses.
func (a *Adapter) reasoningEvent(
	sourceFile, toolID, sessionID, projectRoot, gitBranch, model string,
	ts time.Time,
	msgIdx, blockIdx int,
	thinking string,
) models.ToolEvent {
	scrubbed := a.scrubber.String(thinking)
	preview := truncate(scrubbed, 200)
	hash := shortHash(thinking)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("%s:reasoning:%s:%d:%d:%s", toolID, sessionID, msgIdx, blockIdx, hash),
		SessionID:          sessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          gitBranch,
		Model:              model,
		Tool:               toolID,
		ActionType:         models.ActionTaskComplete,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        toolID + ".reasoning",
		ToolOutput:         a.scrubber.String(contentcap.Cap(thinking, contentcap.DefaultMaxBytes)),
		MessageID:          toolID + ":reasoning:" + hash,
	}
}

// userPromptEvent emits a user_prompt row for a genuine user message in the
// cline/roo conversation history. SourceEventID is content-derivable so the
// store-layer (source_file, source_event_id) upsert dedupes across the
// adapter's full-file re-parses.
func (a *Adapter) userPromptEvent(
	sourceFile, toolID, sessionID, projectRoot, gitBranch, model string,
	ts time.Time,
	msgIdx int,
	prompt string,
) models.ToolEvent {
	preview := truncate(a.scrubber.String(prompt), 200)
	hash := shortHash(prompt)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("%s:user:%s:%d:%s", toolID, sessionID, msgIdx, hash),
		SessionID:          sessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          gitBranch,
		Model:              model,
		Tool:               toolID,
		ActionType:         models.ActionUserPrompt,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "user_message",
		RawToolInput:       a.scrubber.String(prompt),
		MessageID:          toolID + ":user:" + hash,
	}
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

// inferProjectContext extracts the workspace cwd for a Cline / Roo
// session by scanning the two on-disk locations the extensions write.
// The format moved across versions: pre-v3.88.0 wrote a top-level
// `cwd` key on early ui_messages.json entries; v3.88.0+ dropped that
// key entirely and instead embeds the cwd in api_conversation_history.json's
// first user message inside the `<environment_details>` block as
// `Current Working Directory (<path>)`. We try the newer location
// first (the format every current install produces) and fall back
// to ui_messages.json for sessions captured by older Cline versions.
//
// Returns ("", "") when neither file yields a cwd — the watcher still
// stores the action but the store layer's "drop empty ProjectRoot"
// guard then silently discards every event for the session. This was
// the V1 bug surfaced by the 2026-06-06 Windows validation.
//
// The path is normalised via `pathnorm.Normalize` before
// `git.Resolve` so cwd values arriving in foreign shapes (Windows
// drive-letter / file:// URI / surrounding quotes / mixed
// separators) reach git.Resolve as canonical paths. Without this, a
// Windows-side Cline session read from a Linux observer would feed
// e.g. `C:\foo\bar` directly to git.Resolve, which treats the string
// as relative, prepends observer's own CWD, and walks UP — landing
// on observer's own .git in the worst case
// (memory [[feedback_foreign_path_git_resolve]]).
func (a *Adapter) inferProjectContext(path string) (projectRoot, branch string) {
	if cwd := scanAPIHistoryCwd(path); cwd != "" {
		return resolveProjectFromCwd(cwd)
	}
	if cwd := scanUIMessagesCwd(filepath.Join(filepath.Dir(path), "ui_messages.json")); cwd != "" {
		return resolveProjectFromCwd(cwd)
	}
	return "", ""
}

// scanAPIHistoryCwd reads the first cwdScanBytes of
// api_conversation_history.json at path and returns the first
// `Current Working Directory (<path>)` match found inside the env-
// details banner. Empty string when no match or read fails (silent —
// the caller falls through to the ui_messages.json path).
func scanAPIHistoryCwd(apiHistoryPath string) string {
	f, err := os.Open(apiHistoryPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, cwdScanBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ""
	}
	m := cwdEnvDetailsPattern.FindSubmatch(buf[:n])
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// scanUIMessagesCwd is the legacy path — reads ui_messages.json and
// returns the first top-level `cwd` string found across the message
// array. Kept for back-compat with sessions captured by Cline
// VSCode < v3.88.0; on current installs returns "" because the key
// no longer exists.
func scanUIMessagesCwd(uiPath string) string {
	body, err := os.ReadFile(uiPath)
	if err != nil {
		return ""
	}
	var msgs []map[string]any
	if err := json.Unmarshal(body, &msgs); err != nil {
		return ""
	}
	for _, m := range msgs {
		if cwd, ok := m["cwd"].(string); ok && cwd != "" {
			return cwd
		}
	}
	return ""
}

// resolveProjectFromCwd normalises a raw cwd hint and runs git.Resolve
// over it. When git.Resolve finds a repo root the returned root +
// branch are used; otherwise the normalised cwd itself becomes the
// project root with an empty branch (still satisfies the store
// layer's non-empty ProjectRoot requirement).
func resolveProjectFromCwd(cwd string) (string, string) {
	cwd = pathnorm.Normalize(cwd)
	info, err := git.Resolve(cwd)
	if err == nil {
		return info.Root, info.Branch
	}
	return cwd, ""
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
