package openclaw

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter parses OpenClaw's local task/session state. OpenClaw stores durable
// CLI/agent work in ~/.openclaw/tasks/runs.sqlite and keeps a session index
// under ~/.openclaw/agents/<agent>/sessions/sessions.json.
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
func (*Adapter) Name() string { return models.ToolOpenClaw }

// WatchPaths implements adapter.Adapter.
func (a *Adapter) WatchPaths() []string { return a.roots }

// IsSessionFile implements adapter.Adapter.
func (*Adapter) IsSessionFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == "runs.sqlite" || base == "runs.sqlite-wal" || base == "sessions.json" || filepath.Ext(base) == ".jsonl"
}

// ParseSessionFile implements adapter.Adapter.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case base == "runs.sqlite" || base == "runs.sqlite-wal":
		return a.parseTaskRuns(ctx, resolveRunsDB(path), fromOffset)
	case base == "sessions.json":
		return a.parseSessionsIndex(path, fromOffset)
	default:
		return a.parseSessionJSONL(ctx, path, fromOffset)
	}
}

type taskRun struct {
	TaskID          string
	Runtime         string
	SourceID        string
	RequesterKey    string
	OwnerKey        string
	ChildSessionKey string
	AgentID         string
	RunID           string
	Label           string
	Task            string
	Status          string
	CreatedAt       int64
	StartedAt       sql.NullInt64
	EndedAt         sql.NullInt64
	LastEventAt     sql.NullInt64
	Error           sql.NullString
	ProgressSummary sql.NullString
	TerminalSummary sql.NullString
	TerminalOutcome sql.NullString
}

func (a *Adapter) parseTaskRuns(ctx context.Context, dbPath string, fromOffset int64) (adapter.ParseResult, error) {
	latest, err := latestTaskWatermark(ctx, dbPath)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("openclaw.ParseSessionFile: latest task watermark: %w", err)
	}
	res := adapter.ParseResult{NewOffset: latest}
	if latest <= fromOffset {
		return res, nil
	}

	sessionAliases := loadSessionAliases(filepath.Join(filepath.Dir(filepath.Dir(dbPath)), "agents"))

	db, err := openReadOnlyDB(dbPath)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("openclaw.ParseSessionFile: open task DB: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT task_id, runtime, COALESCE(source_id, ''), COALESCE(requester_session_key, ''),
		       owner_key, COALESCE(child_session_key, ''), COALESCE(agent_id, ''),
		       COALESCE(run_id, ''), COALESCE(label, ''), task, status, created_at,
		       started_at, ended_at, last_event_at, error, progress_summary,
		       terminal_summary, terminal_outcome
		  FROM task_runs
		 WHERE COALESCE(last_event_at, ended_at, started_at, created_at) > ?
		 ORDER BY COALESCE(last_event_at, ended_at, started_at, created_at) ASC, task_id ASC`, fromOffset)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("openclaw.ParseSessionFile: query task_runs: %w", err)
	}
	defer rows.Close()

	rootCache := map[string]string{}
	for rows.Next() {
		var tr taskRun
		if err := rows.Scan(
			&tr.TaskID, &tr.Runtime, &tr.SourceID, &tr.RequesterKey, &tr.OwnerKey,
			&tr.ChildSessionKey, &tr.AgentID, &tr.RunID, &tr.Label, &tr.Task, &tr.Status,
			&tr.CreatedAt, &tr.StartedAt, &tr.EndedAt, &tr.LastEventAt, &tr.Error,
			&tr.ProgressSummary, &tr.TerminalSummary, &tr.TerminalOutcome,
		); err != nil {
			return res, err
		}
		if suppressTaskRun(tr, sessionAliases) {
			continue
		}
		alias, _ := findTaskAlias(tr, sessionAliases)
		res.ToolEvents = append(res.ToolEvents, a.taskPromptEvent(dbPath, tr, alias, rootCache))
		if isTerminalStatus(tr.Status) {
			res.ToolEvents = append(res.ToolEvents, a.taskCompleteEvent(dbPath, tr, alias, rootCache))
		}
	}
	return res, rows.Err()
}

// findTaskAlias mirrors suppressTaskRun's key-priority chain but
// returns the matched alias so callers can lift Model / Provider /
// WorkspaceDir off it. Returns (zero, false) when no key matches —
// callers should fall back to "[openclaw]" / empty model.
func findTaskAlias(tr taskRun, aliases map[string]sessionIndexEntry) (sessionIndexEntry, bool) {
	for _, key := range []string{tr.ChildSessionKey, tr.OwnerKey, tr.RequesterKey, tr.RunID, tr.SourceID} {
		if entry, ok := aliases[key]; ok {
			return entry, true
		}
	}
	return sessionIndexEntry{}, false
}

// aliasModel projects a sessionIndexEntry's provider+model into the
// `provider/model` composite that modelName() emits for jsonl rows.
// Empty when the alias has no model fields. Used by sqlite + sessions.json
// paths to stop emitting `Model: ""` on every task_runs / sessions row.
func aliasModel(alias sessionIndexEntry) string {
	provider := firstNonEmpty(alias.ModelProvider, alias.SystemPromptReport.Provider)
	model := firstNonEmpty(alias.Model, alias.SystemPromptReport.Model)
	if provider != "" && model != "" {
		return provider + "/" + model
	}
	return model
}

func (a *Adapter) taskPromptEvent(sourceFile string, tr taskRun, alias sessionIndexEntry, rootCache map[string]string) models.ToolEvent {
	prompt := stripTaskTimestamp(tr.Task)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      "task:" + tr.TaskID + ":prompt",
		SessionID:          sessionID(tr),
		ProjectRoot:        a.resolveProjectRoot(alias.SystemPromptReport.WorkspaceDir, rootCache),
		Timestamp:          millisToTime(tr.CreatedAt),
		Model:              aliasModel(alias),
		Tool:               models.ToolOpenClaw,
		ActionType:         models.ActionUserPrompt,
		Target:             truncate(prompt, 200),
		Success:            true,
		PrecedingReasoning: truncate(prompt, 200),
		RawToolName:        "task_runs.task",
		RawToolInput:       a.scrubber.String(prompt),
		MessageID:          "user:task:" + tr.TaskID,
	}
}

func (a *Adapter) taskCompleteEvent(sourceFile string, tr taskRun, alias sessionIndexEntry, rootCache map[string]string) models.ToolEvent {
	success := strings.EqualFold(tr.Status, "succeeded")
	errMsg := ""
	if !success {
		errMsg = firstNonEmpty(nullString(tr.Error), nullString(tr.TerminalOutcome), nullString(tr.TerminalSummary))
	}
	summary := firstNonEmpty(nullString(tr.ProgressSummary), nullString(tr.TerminalSummary), nullString(tr.TerminalOutcome))
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      "task:" + tr.TaskID + ":complete",
		SessionID:          sessionID(tr),
		ProjectRoot:        a.resolveProjectRoot(alias.SystemPromptReport.WorkspaceDir, rootCache),
		Timestamp:          taskEndTime(tr),
		Model:              aliasModel(alias),
		Tool:               models.ToolOpenClaw,
		ActionType:         models.ActionTaskComplete,
		Target:             tr.Status,
		Success:            success,
		ErrorMessage:       truncate(errMsg, 500),
		DurationMs:         durationMs(tr),
		PrecedingReasoning: truncate(summary, 200),
		RawToolName:        "task_runs.status",
		RawToolInput:       a.scrubber.String(firstNonEmpty(tr.Status, errMsg)),
		ToolOutput:         a.scrubber.String(summary),
		MessageID:          "assistant:task:" + tr.TaskID,
	}
}

type sessionsIndex map[string]sessionIndexEntry

type sessionIndexEntry struct {
	SessionID          string `json:"sessionId"`
	UpdatedAt          int64  `json:"updatedAt"`
	Status             string `json:"status"`
	StartedAt          int64  `json:"startedAt"`
	EndedAt            int64  `json:"endedAt"`
	RuntimeMs          int64  `json:"runtimeMs"`
	ModelProvider      string `json:"modelProvider"`
	Model              string `json:"model"`
	SessionFile        string `json:"sessionFile"`
	SystemPromptReport struct {
		WorkspaceDir string `json:"workspaceDir"`
		SessionKey   string `json:"sessionKey"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
	} `json:"systemPromptReport"`
}

func (a *Adapter) parseSessionsIndex(path string, fromOffset int64) (adapter.ParseResult, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("openclaw.ParseSessionFile: read sessions index: %w", err)
	}
	var idx sessionsIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return adapter.ParseResult{}, fmt.Errorf("openclaw.ParseSessionFile: parse sessions index: %w", err)
	}
	var latest int64
	res := adapter.ParseResult{}
	rootCache := map[string]string{}
	for key, sess := range idx {
		if sess.UpdatedAt > latest {
			latest = sess.UpdatedAt
		}
		if sess.UpdatedAt <= fromOffset || !isTerminalStatus(sess.Status) || strings.TrimSpace(sess.SessionFile) != "" {
			continue
		}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile:    path,
			SourceEventID: "session:" + key + ":complete",
			SessionID:     firstNonEmpty(sess.SessionID, key),
			ProjectRoot:   a.resolveProjectRoot(sess.SystemPromptReport.WorkspaceDir, rootCache),
			Timestamp:     millisToTime(firstNonZero(sess.EndedAt, sess.UpdatedAt)),
			Model:         modelName(&sessionContext{Provider: firstNonEmpty(sess.ModelProvider, sess.SystemPromptReport.Provider), Model: firstNonEmpty(sess.Model, sess.SystemPromptReport.Model)}),
			Tool:          models.ToolOpenClaw,
			ActionType:    models.ActionTaskComplete,
			Target:        sess.Status,
			Success:       strings.EqualFold(sess.Status, "succeeded"),
			DurationMs:    sess.RuntimeMs,
			RawToolName:   "sessions.status",
			MessageID:     "assistant:session:" + key,
		})
	}
	res.NewOffset = latest
	return res, nil
}

type jsonlLine struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Timestamp  string          `json:"timestamp"`
	Cwd        string          `json:"cwd"`
	Provider   string          `json:"provider"`
	ModelID    string          `json:"modelId"`
	Message    openclawMessage `json:"message"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
}

type openclawMessage struct {
	Role         string           `json:"role"`
	Content      []messageContent `json:"content"`
	StopReason   string           `json:"stopReason"`
	API          string           `json:"api"`
	Provider     string           `json:"provider"`
	Model        string           `json:"model"`
	Usage        tokenUsage       `json:"usage"`
	Timestamp    int64            `json:"timestamp"`
	ToolCallID   string           `json:"toolCallId"`
	ToolName     string           `json:"toolName"`
	IsError      bool             `json:"isError"`
	ErrorMessage string           `json:"errorMessage"`
}

type messageContent struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type tokenUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	TotalTokens int64 `json:"totalTokens"`
}

type sessionContext struct {
	SessionID   string
	ProjectRoot string
	Provider    string
	Model       string
}

func (a *Adapter) parseSessionJSONL(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, err
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("openclaw.ParseSessionFile: seek: %w", err)
		}
	}

	res := adapter.ParseResult{NewOffset: fromOffset}
	state := sessionContext{
		SessionID:   strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		ProjectRoot: "[openclaw]",
	}
	applySessionAlias(path, &state, state.SessionID)
	rootCache := map[string]string{}
	// Resolve any aliased ProjectRoot the alias lookup just installed so
	// downstream events get the same git-root treatment as freshly-set cwds.
	if state.ProjectRoot != "" && state.ProjectRoot != "[openclaw]" {
		state.ProjectRoot = a.resolveProjectRoot(state.ProjectRoot, rootCache)
	}
	pending := map[string]int{}
	// seenSystemPrompts dedups ActionSystemPrompt rows by content hash.
	// OpenClaw bootstrap-context:full events can be re-emitted on
	// resume; same content → one row.
	seenSystemPrompts := map[string]bool{}

	scanner := bufio.NewScanner(f)
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	var bytesRead int64 = fromOffset
	lineNum := 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw := scanner.Bytes()
		bytesRead += int64(len(raw) + 1)
		lineNum++
		if len(raw) == 0 {
			continue
		}

		var line jsonlLine
		if err := json.Unmarshal(raw, &line); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
			res.NewOffset = bytesRead
			continue
		}
		res.NewOffset = bytesRead
		ts := parseTimestamp(line.Timestamp)

		switch line.Type {
		case "session":
			if line.ID != "" {
				state.SessionID = line.ID
				applySessionAlias(path, &state, line.ID)
				if state.ProjectRoot != "" && state.ProjectRoot != "[openclaw]" {
					state.ProjectRoot = a.resolveProjectRoot(state.ProjectRoot, rootCache)
				}
			}
			if line.Cwd != "" {
				state.ProjectRoot = a.resolveProjectRoot(line.Cwd, rootCache)
			}
		case "model_change":
			state.Provider = line.Provider
			state.Model = line.ModelID
		case "message":
			a.parseMessageLine(path, line, lineNum, ts, &state, pending, &res)
		case "custom":
			// OpenClaw emits typed `custom` events for runtime
			// notifications. customType="model-snapshot" is redundant
			// with the model_change handler above, so it's a no-op.
			// customType="openclaw:bootstrap-context:full" marks a
			// bootstrap-context load — pre-v1.4.23 silently dropped.
			// Per user direction (2026-05-01): capture event/action
			// info even when no rich body is in the payload. Emit a
			// minimal ActionSystemPrompt row carrying the data field
			// JSON so analysts can detect bootstrap activity.
			if line.CustomType == "openclaw:bootstrap-context:full" && len(line.Data) > 0 {
				body := strings.TrimSpace(string(line.Data))
				if body != "" && body != "null" {
					hash := openclawShortHash("bootstrap:" + body)
					if !seenSystemPrompts[hash] {
						seenSystemPrompts[hash] = true
						preview := "bootstrap-context: " + truncate(body, 180)
						res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
							SourceFile:    path,
							SourceEventID: fmt.Sprintf("sysprompt:bootstrap:%s:L%d", hash, lineNum),
							SessionID:     state.SessionID,
							ProjectRoot:   state.ProjectRoot,
							Timestamp:     ts,
							Model:         modelName(&state),
							Tool:          models.ToolOpenClaw,
							ActionType:    models.ActionSystemPrompt,
							Target:        truncate(preview, 200),
							Success:       true,
							RawToolName:   "system_prompt.bootstrap",
							RawToolInput:  a.scrubber.String(body),
							MessageID:     "system:" + hash,
						})
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("openclaw.ParseSessionFile: scan: %w", err)
	}
	return res, nil
}

func (a *Adapter) parseMessageLine(
	sourceFile string,
	line jsonlLine,
	lineNum int,
	ts time.Time,
	state *sessionContext,
	pending map[string]int,
	res *adapter.ParseResult,
) {
	msg := line.Message
	if msg.Provider != "" {
		state.Provider = msg.Provider
	}
	if msg.Model != "" {
		state.Model = msg.Model
	}
	if msg.Timestamp > 0 {
		ts = millisToTime(msg.Timestamp)
	}

	switch msg.Role {
	case "user":
		text := messageText(msg.Content)
		if text == "" {
			return
		}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile:         sourceFile,
			SourceEventID:      firstNonEmpty(line.ID, fmt.Sprintf("user:L%d", lineNum)),
			SessionID:          state.SessionID,
			ProjectRoot:        state.ProjectRoot,
			Timestamp:          ts,
			Model:              modelName(state),
			Tool:               models.ToolOpenClaw,
			ActionType:         models.ActionUserPrompt,
			Target:             truncate(text, 200),
			Success:            true,
			PrecedingReasoning: truncate(text, 200),
			RawToolName:        "message.user",
			RawToolInput:       a.scrubber.String(text),
			MessageID:          userMessageID(line.ID, lineNum),
		})
	case "assistant":
		assistantMessageID := assistantMessageID(line.ID, lineNum)
		// Track the most recent assistant text/thinking part so each tool
		// call inherits the preamble that introduced it. Mirrors
		// claudecode's per-turn reasoning capture and pi's
		// `messageThinking`. When multiple tool calls share one preamble
		// they all carry the same reasoning string — that's intentional.
		var preceding string
		for _, content := range msg.Content {
			switch content.Type {
			case "text", "thinking":
				if t := strings.TrimSpace(content.Text); t != "" {
					preceding = t
				}
			case "toolCall":
				ev := a.toolCallEvent(sourceFile, line, lineNum, ts, *state, content, assistantMessageID, preceding)
				pending[content.ID] = len(res.ToolEvents)
				res.ToolEvents = append(res.ToolEvents, ev)
			}
		}
		text := messageText(msg.Content)
		if text != "" && msg.StopReason == "stop" {
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:         sourceFile,
				SourceEventID:      firstNonEmpty("complete:"+line.ID, fmt.Sprintf("complete:L%d", lineNum)),
				SessionID:          state.SessionID,
				ProjectRoot:        state.ProjectRoot,
				Timestamp:          ts,
				Model:              modelName(state),
				Tool:               models.ToolOpenClaw,
				ActionType:         models.ActionTaskComplete,
				Target:             "stop",
				Success:            true,
				PrecedingReasoning: truncate(text, 200),
				RawToolName:        "message.assistant.stop",
				ToolOutput:         a.scrubber.String(text),
				MessageID:          assistantMessageID,
			})
		}
		if msg.StopReason == "error" {
			// Upstream API failure: provider rejected the request (model
			// doesn't support tools, rate limit, malformed body, etc.).
			// Pre-v1.4.22 these were silently dropped because the
			// stop-reason gate above only fired for "stop". errorMessage
			// is the verbatim provider response (e.g. `400 {"error":"..."}`).
			errBody := strings.TrimSpace(msg.ErrorMessage)
			if errBody == "" {
				errBody = "(no error message)"
			}
			class := openclawErrorClass(errBody)
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    sourceFile,
				SourceEventID: firstNonEmpty("error:"+line.ID, fmt.Sprintf("error:L%d", lineNum)),
				SessionID:     state.SessionID,
				ProjectRoot:   state.ProjectRoot,
				Timestamp:     ts,
				Model:         modelName(state),
				Tool:          models.ToolOpenClaw,
				ActionType:    models.ActionAPIError,
				Target:        truncate(class, 200),
				Success:       false,
				ErrorMessage:  truncate(a.scrubber.String(errBody), 2048),
				RawToolName:   class,
				MessageID:     assistantMessageID,
			})
		}
		if msg.Usage.Input != 0 || msg.Usage.Output != 0 || msg.Usage.CacheRead != 0 || msg.Usage.CacheWrite != 0 {
			res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
				SourceFile:          sourceFile,
				SourceEventID:       firstNonEmpty("usage:"+line.ID, fmt.Sprintf("usage:L%d", lineNum)),
				SessionID:           state.SessionID,
				ProjectRoot:         state.ProjectRoot,
				Timestamp:           ts,
				Tool:                models.ToolOpenClaw,
				Model:               modelName(state),
				InputTokens:         msg.Usage.Input,
				OutputTokens:        msg.Usage.Output,
				CacheReadTokens:     msg.Usage.CacheRead,
				CacheCreationTokens: msg.Usage.CacheWrite,
				Source:              models.TokenSourceJSONL,
				Reliability:         models.ReliabilityApproximate,
				MessageID:           assistantMessageID,
			})
		}
	case "toolResult":
		idx, ok := pending[msg.ToolCallID]
		if !ok {
			return
		}
		output := messageText(msg.Content)
		res.ToolEvents[idx].ToolOutput = a.scrubber.String(output)
		res.ToolEvents[idx].Success = !msg.IsError
		if msg.IsError {
			res.ToolEvents[idx].ErrorMessage = truncate(output, 500)
		}
	}
}

func (a *Adapter) toolCallEvent(sourceFile string, line jsonlLine, lineNum int, ts time.Time, state sessionContext, content messageContent, messageID, preceding string) models.ToolEvent {
	raw, _ := json.Marshal(content.Arguments)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      firstNonEmpty(content.ID, fmt.Sprintf("tool:%s:L%d", content.Name, lineNum)),
		SessionID:          state.SessionID,
		ProjectRoot:        state.ProjectRoot,
		Timestamp:          ts,
		Model:              modelName(&state),
		Tool:               models.ToolOpenClaw,
		ActionType:         mapToolName(content.Name),
		Target:             truncate(targetFromArgs(content.Arguments, content.Name), 200),
		Success:            true,
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        content.Name,
		RawToolInput:       a.scrubber.RawJSON(raw),
		MessageID:          messageID,
	}
}

func applySessionAlias(path string, state *sessionContext, sessionID string) {
	if state == nil {
		return
	}
	alias, ok := lookupSessionAlias(path, sessionID)
	if !ok {
		return
	}
	if alias.SessionID != "" {
		state.SessionID = alias.SessionID
	}
	if alias.ProjectRoot != "" {
		state.ProjectRoot = alias.ProjectRoot
	}
	if alias.Provider != "" {
		state.Provider = alias.Provider
	}
	if alias.Model != "" {
		state.Model = alias.Model
	}
}

func lookupSessionAlias(path string, sessionID string) (sessionContext, bool) {
	indexPath := filepath.Join(filepath.Dir(path), "sessions.json")
	body, err := os.ReadFile(indexPath)
	if err != nil {
		return sessionContext{}, false
	}
	var idx sessionsIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return sessionContext{}, false
	}
	base := filepath.Base(path)
	for key, sess := range idx {
		if filepath.Base(sess.SessionFile) == base || sess.SessionID == sessionID || key == sessionID {
			return sessionContext{
				SessionID:   firstNonEmpty(key, sess.SystemPromptReport.SessionKey, sess.SessionID),
				ProjectRoot: firstNonEmpty(sess.SystemPromptReport.WorkspaceDir),
				Provider:    firstNonEmpty(sess.ModelProvider, sess.SystemPromptReport.Provider),
				Model:       firstNonEmpty(sess.Model, sess.SystemPromptReport.Model),
			}, true
		}
	}
	return sessionContext{}, false
}

func loadSessionAliases(agentsRoot string) map[string]sessionIndexEntry {
	aliases := map[string]sessionIndexEntry{}
	entries, err := filepath.Glob(filepath.Join(agentsRoot, "*", "sessions", "sessions.json"))
	if err != nil {
		return aliases
	}
	for _, indexPath := range entries {
		body, err := os.ReadFile(indexPath)
		if err != nil {
			continue
		}
		var idx sessionsIndex
		if err := json.Unmarshal(body, &idx); err != nil {
			continue
		}
		for key, entry := range idx {
			aliases[key] = entry
			if entry.SessionID != "" {
				aliases[entry.SessionID] = entry
			}
		}
	}
	return aliases
}

func suppressTaskRun(tr taskRun, aliases map[string]sessionIndexEntry) bool {
	for _, key := range []string{tr.ChildSessionKey, tr.OwnerKey, tr.RequesterKey, tr.RunID, tr.SourceID} {
		if entry, ok := aliases[key]; ok && strings.TrimSpace(entry.SessionFile) != "" {
			return true
		}
	}
	return false
}

func messageText(contents []messageContent) string {
	var parts []string
	for _, c := range contents {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// openclawShortHash returns a short hex digest of s for use in
// SourceEventID / MessageID prefixes. Matches the cursor /
// claudecode adapters' shortHash convention (12 hex chars).
func openclawShortHash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// openclawErrorClass extracts a short discriminator from an
// errorMessage body. Matches "<status_code> ..." prefixes (e.g.
// "400 {...}" → "http_400") and falls back to "api_error" otherwise.
// Mirrors claudecode / codex api_error class conventions so dashboards
// can group related failure classes across adapters.
func openclawErrorClass(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return "api_error"
	}
	// Try "NNN ..." status-code prefix.
	if i := strings.IndexByte(body, ' '); i > 0 && i <= 4 {
		prefix := body[:i]
		allDigits := true
		for _, r := range prefix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return "http_" + prefix
		}
	}
	return "api_error"
}

func mapToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "cat", "view":
		return models.ActionReadFile
	case "write", "create":
		return models.ActionWriteFile
	case "edit", "patch", "replace":
		return models.ActionEditFile
	case "exec", "bash", "shell", "command":
		return models.ActionRunCommand
	case "web_fetch":
		return models.ActionWebFetch
	case "browser":
		return models.ActionBrowserAction
	case "memory_search":
		return models.ActionSearchText
	// sessions_spawn launches a sub-agent — semantically equivalent to
	// Claude Code's Agent tool. Was bucketed with the other sessions_*
	// MCP-style tools by mistake; the name "spawn" is the giveaway.
	// Promoting it to ActionSpawnSubagent so dashboard fan-out counts
	// pick it up the same way they pick up claudecode's Agent and
	// opencode's `task`/`agent`/`subagent` tools.
	case "sessions_spawn":
		return models.ActionSpawnSubagent
	case "sessions_list", "sessions_history", "sessions_send", "agents_list", "gateway":
		return models.ActionMCPCall
	default:
		return models.ActionUnknown
	}
}

// resolveProjectRoot turns a recorded cwd into a stable project root.
// Mirrors the codex / opencode pattern: empty input yields the
// "[openclaw]" placeholder so historical rows continue to coalesce;
// real paths inside a git working tree resolve to the repo root.
// The cache lives for one parse call.
func (a *Adapter) resolveProjectRoot(cwd string, cache map[string]string) string {
	if cwd == "" {
		return "[openclaw]"
	}
	if root, ok := cache[cwd]; ok {
		return root
	}
	info, err := git.Resolve(cwd)
	if err != nil {
		cache[cwd] = cwd
		return cwd
	}
	cache[cwd] = info.Root
	return info.Root
}

func targetFromArgs(args map[string]any, fallback string) string {
	for _, key := range []string{"path", "file", "filePath", "command", "cmd", "url", "query", "sessionId"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fallback
}

func modelName(state *sessionContext) string {
	if state == nil {
		return ""
	}
	if state.Provider != "" && state.Model != "" {
		return state.Provider + "/" + state.Model
	}
	return state.Model
}

func latestTaskWatermark(ctx context.Context, path string) (int64, error) {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var latest int64
	row := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(COALESCE(last_event_at, ended_at, started_at, created_at)), 0) FROM task_runs`)
	if err := row.Scan(&latest); err != nil {
		return 0, err
	}
	return latest, nil
}

func openReadOnlyDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)", filepath.ToSlash(path))
	return sql.Open("sqlite", dsn)
}

func resolveRunsDB(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if base == "runs.sqlite-wal" || base == "runs.sqlite-shm" {
		return filepath.Join(filepath.Dir(path), "runs.sqlite")
	}
	return path
}

// defaultRoots returns OpenClaw's tasks + agents dirs under every
// cross-mount-resolved $HOME so observer in WSL2 picks up data from
// /mnt/c/Users/<u>/.openclaw (and vice-versa). Subpaths are uniform
// across OSes — OpenClaw uses ~/.openclaw on every host.
func defaultRoots() []string {
	var roots []string
	for _, h := range crossmount.AllHomes() {
		roots = append(roots,
			filepath.Join(h.Path, ".openclaw", "tasks"),
			filepath.Join(h.Path, ".openclaw", "agents"),
		)
	}
	return roots
}

var taskPrefixRE = regexp.MustCompile(`^\[[^\]]+\]\s*`)

func stripTaskTimestamp(s string) string {
	return strings.TrimSpace(taskPrefixRE.ReplaceAllString(s, ""))
}

func sessionID(tr taskRun) string {
	return firstNonEmpty(tr.ChildSessionKey, tr.OwnerKey, tr.RequesterKey, tr.RunID, tr.SourceID, tr.TaskID)
}

func taskEndTime(tr taskRun) time.Time {
	return millisToTime(firstNonZero(nullInt(tr.EndedAt), nullInt(tr.LastEventAt), nullInt(tr.StartedAt), tr.CreatedAt))
}

func durationMs(tr taskRun) int64 {
	start := nullInt(tr.StartedAt)
	end := firstNonZero(nullInt(tr.EndedAt), nullInt(tr.LastEventAt))
	if start <= 0 || end <= start {
		return 0
	}
	return end - start
}

func isTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "failed", "timed_out", "cancelled", "lost":
		return true
	default:
		return false
	}
}

func nullInt(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

func nullString(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func millisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func parseTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func userMessageID(id string, lineNum int) string {
	return "user:" + firstNonEmpty(id, fmt.Sprintf("L%d", lineNum))
}

func assistantMessageID(id string, lineNum int) string {
	return firstNonEmpty(id, fmt.Sprintf("assistant:L%d", lineNum))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
