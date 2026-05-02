package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter parses OpenCode's SQLite state store. OpenCode persists user
// messages, tool calls, and assistant completions in opencode.db, which gives
// us richer capture than the coarse desktop snapshot files.
type Adapter struct {
	scrubber *scrub.Scrubber
	roots    []string
}

// New returns an adapter with platform-specific default roots.
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
func (*Adapter) Name() string { return models.ToolOpenCode }

// WatchPaths implements adapter.Adapter.
func (a *Adapter) WatchPaths() []string { return a.roots }

// IsSessionFile implements adapter.Adapter.
func (*Adapter) IsSessionFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == "opencode.db" || base == "opencode.db-wal"
}

// ParseSessionFile implements adapter.Adapter.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	dbPath := resolveDBPath(path)
	latest, err := latestWatermark(ctx, dbPath)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: latest watermark: %w", err)
	}
	res := adapter.ParseResult{NewOffset: latest}
	if latest <= fromOffset {
		return res, nil
	}

	database, err := openReadOnlyDB(dbPath)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: open: %w", err)
	}
	defer database.Close()

	rootCache := map[string]string{}
	prompts, err := a.loadUserPromptEvents(ctx, database, dbPath, fromOffset, rootCache)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: prompts: %w", err)
	}
	tools, err := a.loadToolEvents(ctx, database, dbPath, fromOffset, rootCache)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: tools: %w", err)
	}
	completions, err := a.loadCompletionEvents(ctx, database, dbPath, fromOffset, rootCache)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: completions: %w", err)
	}
	subtasks, err := a.loadSubtaskEvents(ctx, database, dbPath, fromOffset, rootCache)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: subtasks: %w", err)
	}
	todos, err := a.loadTodoEvents(ctx, database, dbPath, fromOffset, rootCache)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: todos: %w", err)
	}
	tokens, err := a.loadTokenEvents(ctx, database, dbPath, fromOffset, rootCache)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("opencode.ParseSessionFile: tokens: %w", err)
	}
	res.ToolEvents = append(res.ToolEvents, prompts...)
	res.ToolEvents = append(res.ToolEvents, tools...)
	res.ToolEvents = append(res.ToolEvents, completions...)
	res.ToolEvents = append(res.ToolEvents, subtasks...)
	res.ToolEvents = append(res.ToolEvents, todos...)
	res.TokenEvents = append(res.TokenEvents, tokens...)
	return res, nil
}

type messageRow struct {
	ID         string
	SessionID  string
	Directory  string
	TimeCreate int64
	TimeUpdate int64
	Data       string
}

type partRow struct {
	ID         string
	MessageID  string
	SessionID  string
	Directory  string
	TimeCreate int64
	TimeUpdate int64
	Data       string
	Message    string
}

type messageData struct {
	Role  string `json:"role"`
	Agent string `json:"agent"`
	Model struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
	} `json:"model"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Path       struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Finish string `json:"finish"`
	// Tokens + Cost are populated on assistant messages by OpenCode's
	// session writer, mirroring the upstream provider's usage envelope.
	// Confirmed against the InfoData zod schema in opencode's
	// packages/opencode/src/session/message.ts:
	//   tokens: { input, output, reasoning, cache: { read, write } }
	//   cost:   number (USD)
	// All zero when the message hasn't completed (assistant Finish=="")
	// or the role isn't assistant — feeds the loadTokenEvents emitter.
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Cost float64 `json:"cost"`
}

type textPartData struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolPartData struct {
	Type   string `json:"type"`
	Tool   string `json:"tool"`
	CallID string `json:"callID"`
	State  struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Output   string          `json:"output"`
		Metadata struct {
			Output      string `json:"output"`
			Exit        int    `json:"exit"`
			Description string `json:"description"`
			FilePath    string `json:"filepath"`
			Truncated   bool   `json:"truncated"`
		} `json:"metadata"`
		Title string `json:"title"`
		Time  struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time"`
	} `json:"state"`
}

// subtaskPartData mirrors OpenCode's SubtaskPart schema (per
// packages/opencode/src/session/message-v2.ts:228–240). Emitted by
// the parent's message when it invokes a subagent — the prompt,
// description, agent name, and optional model for the spawned
// subagent. The actual sub-agent runs in a child session linked
// via session.parent_id (which we'll wire into our sessions table
// in v1.5.0).
type subtaskPartData struct {
	Type        string `json:"type"`
	Prompt      string `json:"prompt"`
	Description string `json:"description"`
	Agent       string `json:"agent"`
	Model       struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
	} `json:"model"`
	Command string `json:"command"`
	Time    struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type toolInput struct {
	Command  string `json:"command"`
	FilePath string `json:"filePath"`
}

func (a *Adapter) loadUserPromptEvents(ctx context.Context, db *sql.DB, sourceFile string, fromOffset int64, rootCache map[string]string) ([]models.ToolEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.session_id, COALESCE(s.directory, ''), m.time_created, m.time_updated, m.data
		  FROM message m
		  LEFT JOIN session s ON s.id = m.session_id
		 WHERE m.time_updated > ?
		   AND json_extract(m.data, '$.role') = 'user'
		 ORDER BY m.time_updated ASC, m.id ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ToolEvent
	for rows.Next() {
		var row messageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Directory, &row.TimeCreate, &row.TimeUpdate, &row.Data); err != nil {
			return nil, err
		}
		ev, ok, err := a.userPromptEvent(ctx, db, sourceFile, row, rootCache)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ev)
		}
	}
	return out, rows.Err()
}

func (a *Adapter) userPromptEvent(ctx context.Context, db *sql.DB, sourceFile string, row messageRow, rootCache map[string]string) (models.ToolEvent, bool, error) {
	var msg messageData
	if err := json.Unmarshal([]byte(row.Data), &msg); err != nil {
		return models.ToolEvent{}, false, nil
	}

	partRows, err := db.QueryContext(ctx, `
		SELECT data
		  FROM part
		 WHERE message_id = ?
		 ORDER BY time_created ASC, id ASC`, row.ID)
	if err != nil {
		return models.ToolEvent{}, false, err
	}
	defer partRows.Close()

	var promptParts []string
	for partRows.Next() {
		var raw string
		if err := partRows.Scan(&raw); err != nil {
			return models.ToolEvent{}, false, err
		}
		var part textPartData
		if err := json.Unmarshal([]byte(raw), &part); err != nil {
			continue
		}
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			promptParts = append(promptParts, part.Text)
		}
	}
	if err := partRows.Err(); err != nil {
		return models.ToolEvent{}, false, err
	}

	prompt := strings.TrimSpace(strings.Join(promptParts, "\n"))
	if prompt == "" {
		return models.ToolEvent{}, false, nil
	}
	when := millisToTime(msg.Time.Created)
	if when.IsZero() {
		when = millisToTime(row.TimeCreate)
	}
	project := a.resolveProjectRoot(firstNonEmpty(msg.Path.Cwd, row.Directory), rootCache)
	model := firstNonEmpty(msg.Model.ModelID, msg.ModelID, msg.Agent)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      "message:" + row.ID,
		SessionID:          row.SessionID,
		ProjectRoot:        project,
		Timestamp:          chooseTime(when, time.Time{}, 0),
		Model:              model,
		Tool:               models.ToolOpenCode,
		ActionType:         models.ActionUserPrompt,
		Target:             truncate(prompt, 200),
		Success:            true,
		PrecedingReasoning: truncate(prompt, 200),
		RawToolName:        "chat.message",
		RawToolInput:       a.scrubber.String(prompt),
		MessageID:          "user:" + row.ID,
	}, true, nil
}

func (a *Adapter) loadToolEvents(ctx context.Context, db *sql.DB, sourceFile string, fromOffset int64, rootCache map[string]string) ([]models.ToolEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.message_id, p.session_id, COALESCE(s.directory, ''), p.time_created, p.time_updated, p.data, m.data
		  FROM part p
		  JOIN message m ON m.id = p.message_id
		  LEFT JOIN session s ON s.id = p.session_id
		 WHERE p.time_updated > ?
		   AND json_extract(p.data, '$.type') = 'tool'
		 ORDER BY p.time_updated ASC, p.id ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ToolEvent
	for rows.Next() {
		var row partRow
		if err := rows.Scan(&row.ID, &row.MessageID, &row.SessionID, &row.Directory, &row.TimeCreate, &row.TimeUpdate, &row.Data, &row.Message); err != nil {
			return nil, err
		}
		ev, ok := a.toolEvent(sourceFile, row, rootCache)
		if ok {
			out = append(out, ev)
		}
	}
	return out, rows.Err()
}

func (a *Adapter) toolEvent(sourceFile string, row partRow, rootCache map[string]string) (models.ToolEvent, bool) {
	var msg messageData
	if err := json.Unmarshal([]byte(row.Message), &msg); err != nil {
		return models.ToolEvent{}, false
	}
	var part toolPartData
	if err := json.Unmarshal([]byte(row.Data), &part); err != nil {
		return models.ToolEvent{}, false
	}
	if part.Type != "tool" {
		return models.ToolEvent{}, false
	}

	actionType, target, success, errMsg := mapTool(part)
	when := millisToTime(part.State.Time.Start)
	if when.IsZero() {
		when = millisToTime(row.TimeCreate)
	}
	project := a.resolveProjectRoot(firstNonEmpty(msg.Path.Cwd, row.Directory), rootCache)
	// Drop msg.Agent as a model fallback — `agent` is the OpenCode
	// agent identity (build / plan / explore / build-subagent / etc.),
	// NOT a model name. Letting it leak into the model column polluted
	// rollups for system messages where modelID was empty.
	model := firstNonEmpty(msg.ModelID, msg.Model.ModelID)

	rawInput := string(part.State.Input)
	if a.scrubber != nil {
		rawInput = a.scrubber.RawJSON(part.State.Input)
	}
	// ToolOutput: capture the tool result body for every tool, not only
	// failed bash commands. State.Output is OpenCode's canonical output
	// slot; Metadata.Output is the bash-specific stdout/stderr fallback.
	output := firstNonEmpty(part.State.Output, part.State.Metadata.Output)
	if a.scrubber != nil {
		output = a.scrubber.String(output)
	}
	// DurationMs: derive from the part's own start/end timestamps when
	// both are present. Source carries epoch-millis so this is an exact
	// subtract.
	var durationMs int64
	if part.State.Time.Start > 0 && part.State.Time.End > part.State.Time.Start {
		durationMs = part.State.Time.End - part.State.Time.Start
	}
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      "part:" + row.ID,
		SessionID:          row.SessionID,
		ProjectRoot:        project,
		Timestamp:          chooseTime(when, time.Time{}, 0),
		Model:              model,
		Tool:               models.ToolOpenCode,
		ActionType:         actionType,
		Target:             truncate(target, 200),
		Success:            success,
		ErrorMessage:       truncate(errMsg, 500),
		DurationMs:         durationMs,
		PrecedingReasoning: truncate(strings.TrimSpace(part.State.Title), 200),
		RawToolName:        part.Tool,
		RawToolInput:       rawInput,
		ToolOutput:         output,
		MessageID:          row.MessageID,
	}, true
}

func (a *Adapter) loadCompletionEvents(ctx context.Context, db *sql.DB, sourceFile string, fromOffset int64, rootCache map[string]string) ([]models.ToolEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.session_id, COALESCE(s.directory, ''), m.time_created, m.time_updated, m.data
		  FROM message m
		  LEFT JOIN session s ON s.id = m.session_id
		 WHERE m.time_updated > ?
		   AND json_extract(m.data, '$.role') = 'assistant'
		   AND json_extract(m.data, '$.finish') = 'stop'
		 ORDER BY m.time_updated ASC, m.id ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ToolEvent
	for rows.Next() {
		var row messageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Directory, &row.TimeCreate, &row.TimeUpdate, &row.Data); err != nil {
			return nil, err
		}
		ev, ok := a.completionEvent(sourceFile, row, rootCache)
		if ok {
			out = append(out, ev)
		}
	}
	return out, rows.Err()
}

func (a *Adapter) completionEvent(sourceFile string, row messageRow, rootCache map[string]string) (models.ToolEvent, bool) {
	var msg messageData
	if err := json.Unmarshal([]byte(row.Data), &msg); err != nil {
		return models.ToolEvent{}, false
	}
	project := a.resolveProjectRoot(firstNonEmpty(msg.Path.Cwd, row.Directory), rootCache)
	// Drop msg.Agent as a model fallback — `agent` is the OpenCode
	// agent identity (build / plan / explore / build-subagent / etc.),
	// NOT a model name. Letting it leak into the model column polluted
	// rollups for system messages where modelID was empty.
	model := firstNonEmpty(msg.ModelID, msg.Model.ModelID)
	when := millisToTime(msg.Time.Completed)
	if when.IsZero() {
		when = millisToTime(row.TimeUpdate)
	}
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: "complete:" + row.ID,
		SessionID:     row.SessionID,
		ProjectRoot:   project,
		Timestamp:     chooseTime(when, time.Time{}, 0),
		Model:         model,
		Tool:          models.ToolOpenCode,
		ActionType:    models.ActionTaskComplete,
		Target:        firstNonEmpty(msg.Finish, "stop"),
		Success:       true,
		RawToolName:   "assistant.stop",
		MessageID:     row.ID,
	}, true
}

// loadTokenEvents reads OpenCode's per-message token usage from the
// assistant rows in the message table and emits one TokenEvent per
// assistant message. The token counts live in the JSON data blob —
// confirmed against OpenCode's InfoData zod schema in
// packages/opencode/src/session/message.ts:
//
//	tokens: { input, output, reasoning, cache: { read, write } }
//	cost:   number (USD)
//
// Skips rows where assistant hasn't finished (finish != 'stop') or
// no tokens are present (zero across the bundle) — those are still
// in-progress turns.
func (a *Adapter) loadTokenEvents(ctx context.Context, db *sql.DB, sourceFile string, fromOffset int64, rootCache map[string]string) ([]models.TokenEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.session_id, COALESCE(s.directory, ''), m.time_created, m.time_updated, m.data
		  FROM message m
		  LEFT JOIN session s ON s.id = m.session_id
		 WHERE m.time_updated > ?
		   AND json_extract(m.data, '$.role') = 'assistant'
		 ORDER BY m.time_updated ASC, m.id ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.TokenEvent
	for rows.Next() {
		var row messageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Directory, &row.TimeCreate, &row.TimeUpdate, &row.Data); err != nil {
			return nil, err
		}
		ev, ok := a.tokenEvent(sourceFile, row, rootCache)
		if ok {
			out = append(out, ev)
		}
	}
	return out, rows.Err()
}

// tokenEvent extracts a TokenEvent from one assistant message row.
// Returns ok=false when the row carries no token data — early
// in-progress turns, or non-assistant rows that slipped past the
// SQL filter.
func (a *Adapter) tokenEvent(sourceFile string, row messageRow, rootCache map[string]string) (models.TokenEvent, bool) {
	var msg messageData
	if err := json.Unmarshal([]byte(row.Data), &msg); err != nil {
		return models.TokenEvent{}, false
	}
	// Skip if no token data — in-progress turn or empty assistant slot.
	if msg.Tokens.Input == 0 && msg.Tokens.Output == 0 &&
		msg.Tokens.Cache.Read == 0 && msg.Tokens.Cache.Write == 0 &&
		msg.Tokens.Reasoning == 0 {
		return models.TokenEvent{}, false
	}
	project := a.resolveProjectRoot(firstNonEmpty(msg.Path.Cwd, row.Directory), rootCache)
	// Drop msg.Agent as a model fallback — `agent` is the OpenCode
	// agent identity (build / plan / explore / build-subagent / etc.),
	// NOT a model name. Letting it leak into the model column polluted
	// rollups for system messages where modelID was empty.
	model := firstNonEmpty(msg.ModelID, msg.Model.ModelID)
	when := millisToTime(msg.Time.Completed)
	if when.IsZero() {
		when = millisToTime(row.TimeUpdate)
	}
	return models.TokenEvent{
		SourceFile:          sourceFile,
		SourceEventID:       "tokens:" + row.ID,
		SessionID:           row.SessionID,
		ProjectRoot:         project,
		Timestamp:           when,
		Tool:                models.ToolOpenCode,
		Model:               model,
		InputTokens:         msg.Tokens.Input,
		OutputTokens:        msg.Tokens.Output,
		CacheReadTokens:     msg.Tokens.Cache.Read,
		CacheCreationTokens: msg.Tokens.Cache.Write,
		ReasoningTokens:     msg.Tokens.Reasoning,
		EstimatedCostUSD:    msg.Cost,
		// Source/Reliability: OpenCode persists the upstream provider's
		// usage envelope verbatim (no streaming-placeholder hack like
		// Claude Code's JSONL has), so the data is more trustworthy than
		// `unreliable` but still hasn't been verified against an upstream
		// invoice — call it `approximate`.
		Source:      models.TokenSourceJSONL,
		Reliability: models.ReliabilityApproximate,
		MessageID:   row.ID,
	}, true
}

// loadSubtaskEvents reads OpenCode's `subtask` parts and emits one
// ActionSpawnSubagent per — the parent message invoked a sub-agent.
// The actual sub-agent runs in a child session linked via
// session.parent_id; that linkage is captured in v1.5.0. For now,
// surfacing the parent-side spawn lets the dashboard count fan-out
// the same way Claude Code's Agent tool does.
func (a *Adapter) loadSubtaskEvents(ctx context.Context, db *sql.DB, sourceFile string, fromOffset int64, rootCache map[string]string) ([]models.ToolEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.message_id, p.session_id, COALESCE(s.directory, ''),
		       p.time_created, p.time_updated, p.data, m.data
		  FROM part p
		  JOIN message m ON m.id = p.message_id
		  LEFT JOIN session s ON s.id = p.session_id
		 WHERE p.time_updated > ?
		   AND json_extract(p.data, '$.type') = 'subtask'
		 ORDER BY p.time_updated ASC, p.id ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ToolEvent
	for rows.Next() {
		var row partRow
		if err := rows.Scan(&row.ID, &row.MessageID, &row.SessionID, &row.Directory, &row.TimeCreate, &row.TimeUpdate, &row.Data, &row.Message); err != nil {
			return nil, err
		}
		var msg messageData
		if err := json.Unmarshal([]byte(row.Message), &msg); err != nil {
			continue
		}
		var sub subtaskPartData
		if err := json.Unmarshal([]byte(row.Data), &sub); err != nil {
			continue
		}
		if sub.Type != "subtask" {
			continue
		}
		// Target is the spawned agent name (Build/Plan/Explore/custom).
		// RawToolName captures the description so the Actions table
		// shows what the parent asked the subagent to do.
		project := a.resolveProjectRoot(firstNonEmpty(msg.Path.Cwd, row.Directory), rootCache)
		// Prefer the spawned subagent's model when set; falls back to
		// the parent message's model.
		model := firstNonEmpty(sub.Model.ModelID, msg.ModelID, msg.Model.ModelID)
		target := firstNonEmpty(sub.Agent, "subagent")
		when := millisToTime(sub.Time.Created)
		if when.IsZero() {
			when = millisToTime(row.TimeCreate)
		}
		out = append(out, models.ToolEvent{
			SourceFile:    sourceFile,
			SourceEventID: "subtask:" + row.ID,
			SessionID:     row.SessionID,
			ProjectRoot:   project,
			Timestamp:     when,
			Model:         model,
			Tool:          models.ToolOpenCode,
			ActionType:    models.ActionSpawnSubagent,
			Target:        target,
			Success:       true,
			RawToolName:   "subtask",
			RawToolInput:  truncate(firstNonEmpty(sub.Description, sub.Prompt), 200),
			MessageID:     row.MessageID,
		})
	}
	return out, rows.Err()
}

// loadTodoEvents reads the `todo` table — each row is one entry in
// the agent's structured task list (status: pending/in_progress/
// completed). Emits one ActionTodoUpdate per row that has changed
// since fromOffset, mirroring how Claude Code's TaskCreate /
// TaskUpdate / TaskList tools are tagged in the claudecode adapter.
//
// The todo table has a composite PK of (session_id, position), no
// stable id; we synthesize a deterministic SourceEventID from
// session_id + position so re-ingest is INSERT-OR-IGNORE-safe.
//
// Tolerant of older OpenCode schemas that lack the todo table —
// the SQL error gets swallowed and the function returns an empty
// slice rather than failing the whole parse pass.
func (a *Adapter) loadTodoEvents(ctx context.Context, db *sql.DB, sourceFile string, fromOffset int64, rootCache map[string]string) ([]models.ToolEvent, error) {
	if !tableExists(ctx, db, "todo") {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.session_id, t.position, t.content, t.status, t.priority,
		       t.time_created, t.time_updated, COALESCE(s.directory, '')
		  FROM todo t
		  LEFT JOIN session s ON s.id = t.session_id
		 WHERE t.time_updated > ?
		 ORDER BY t.time_updated ASC, t.position ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ToolEvent
	for rows.Next() {
		var sessionID, content, status, priority, dir string
		var position int
		var tCreated, tUpdated int64
		if err := rows.Scan(&sessionID, &position, &content, &status, &priority, &tCreated, &tUpdated, &dir); err != nil {
			return nil, err
		}
		when := millisToTime(tUpdated)
		if when.IsZero() {
			when = millisToTime(tCreated)
		}
		eventID := fmt.Sprintf("todo:%s:%d:%d", sessionID, position, tUpdated)
		out = append(out, models.ToolEvent{
			SourceFile:    sourceFile,
			SourceEventID: eventID,
			SessionID:     sessionID,
			ProjectRoot:   a.resolveProjectRoot(dir, rootCache),
			Timestamp:     when,
			Tool:          models.ToolOpenCode,
			ActionType:    models.ActionTodoUpdate,
			// Target carries the status so dashboards can filter by
			// pending vs completed without having to parse RawToolInput.
			Target:       status,
			Success:      true,
			RawToolName:  "todo." + status,
			RawToolInput: truncate(content, 200),
		})
	}
	return out, rows.Err()
}

func mapTool(part toolPartData) (actionType, target string, success bool, errMsg string) {
	var input toolInput
	_ = json.Unmarshal(part.State.Input, &input)

	actionType = models.ActionUnknown
	target = firstNonEmpty(input.Command, input.FilePath, part.State.Metadata.FilePath, part.State.Title, part.Tool)
	success = part.State.Status == "" || strings.EqualFold(part.State.Status, "completed")
	if part.Tool == "bash" {
		success = success && part.State.Metadata.Exit == 0
		if !success {
			errMsg = firstNonEmpty(part.State.Output, part.State.Metadata.Output)
		}
	}

	switch strings.ToLower(strings.TrimSpace(part.Tool)) {
	case "bash", "shell", "command":
		actionType = models.ActionRunCommand
		target = firstNonEmpty(input.Command, part.State.Title, part.Tool)
	case "read", "cat", "view":
		actionType = models.ActionReadFile
		target = firstNonEmpty(input.FilePath, part.State.Metadata.FilePath, part.State.Title, part.Tool)
	case "write", "create":
		actionType = models.ActionWriteFile
		target = firstNonEmpty(input.FilePath, part.State.Metadata.FilePath, part.State.Title, part.Tool)
	case "edit", "patch", "replace", "multiedit", "applypatch":
		actionType = models.ActionEditFile
		target = firstNonEmpty(input.FilePath, part.State.Metadata.FilePath, part.State.Title, part.Tool)
	case "grep", "search", "rg":
		actionType = models.ActionSearchText
	case "glob", "find", "ls":
		actionType = models.ActionSearchFiles
	case "webfetch", "fetch", "http":
		actionType = models.ActionWebFetch
		target = firstNonEmpty(part.State.Title, part.Tool)
	case "websearch":
		actionType = models.ActionWebSearch
		target = firstNonEmpty(part.State.Title, part.Tool)
	case "task", "agent", "subagent":
		// OpenCode's Task tool launches a subagent; same semantic as
		// Claude Code's Agent tool. Tag spawn_subagent so the dashboard
		// counts fan-out separately from regular tool work.
		actionType = models.ActionSpawnSubagent
		target = firstNonEmpty(part.State.Title, part.Tool)
	case "todoread", "todowrite", "todo":
		// Mirrors Claude Code's TaskCreate/TaskUpdate todo-list tools
		// (mapped to ActionTodoUpdate in the claudecode adapter).
		actionType = models.ActionTodoUpdate
		target = firstNonEmpty(part.State.Title, part.Tool)
	default:
		if strings.Contains(strings.ToLower(part.Tool), "mcp") {
			actionType = models.ActionMCPCall
		}
	}
	return actionType, target, success, errMsg
}

// resolveProjectRoot turns an OpenCode-recorded cwd (or session.directory)
// into a stable project root. Mirrors the codex adapter's pattern:
//
//   - empty cwd → "[opencode]" placeholder (matches pre-parity behavior so
//     existing "[opencode]" rows continue to coalesce).
//   - real cwd inside a git working tree → the git root (so a session that
//     started in a subdirectory groups under the same project as one that
//     started at the repo root).
//   - real cwd outside any git tree → the cwd itself (post-symlink).
//
// The cache lives for one ParseSessionFile call; same cwd resolves once.
func (a *Adapter) resolveProjectRoot(cwd string, cache map[string]string) string {
	if cwd == "" {
		return "[opencode]"
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

func latestWatermark(ctx context.Context, path string) (int64, error) {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var latest int64
	row := db.QueryRowContext(ctx, `
		SELECT MAX(v) FROM (
			SELECT COALESCE(MAX(time_updated), 0) AS v FROM message
			UNION ALL
			SELECT COALESCE(MAX(time_updated), 0) AS v FROM part
			UNION ALL
			SELECT COALESCE(MAX(time_updated), 0) AS v FROM session
		)`)
	if err := row.Scan(&latest); err != nil {
		return 0, err
	}
	return latest, nil
}

func openReadOnlyDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)", filepath.ToSlash(path))
	return sql.Open("sqlite", dsn)
}

func resolveDBPath(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if base == "opencode.db-wal" || base == "opencode.db-shm" {
		return filepath.Join(filepath.Dir(path), "opencode.db")
	}
	return path
}

func millisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func chooseTime(primary, fallback time.Time, delta time.Duration) time.Time {
	if !primary.IsZero() {
		return primary
	}
	if fallback.IsZero() {
		return time.Now().UTC().Add(delta)
	}
	return fallback.Add(delta)
}

// defaultRoots covers both OpenCode variants in the wild today:
//
//   - sst/opencode (opencode.ai) — current canonical CLI; stores at
//     ~/.opencode/opencode.db. This is the path the upstream docs and
//     the `opencode stats` CLI advertise.
//   - ai.opencode.desktop — the older desktop app variant; per-OS
//     XDG-ish paths kept for back-compat with installs that haven't
//     migrated.
//   - ~/.local/share/opencode — XDG_DATA_HOME fallback used by some
//     packagings of either variant.
//
// Roots are emitted under every cross-mount-resolved $HOME so observer
// in WSL2 picks up an OpenCode install on the Windows side (and
// vice-versa). The desktop-variant subpath branches on h.OS because
// each platform has a different conventional location. The canonical
// ".opencode" and XDG ".local/share/opencode" are emitted for every
// home — they're tolerated even on Windows installs that don't use
// them (the IsSessionFile check ignores absent files).
func defaultRoots() []string {
	var roots []string
	for _, h := range crossmount.AllHomes() {
		roots = append(roots,
			filepath.Join(h.Path, ".opencode"),
			filepath.Join(h.Path, ".local", "share", "opencode"),
		)
		switch h.OS {
		case crossmount.OSWindows:
			roots = append(roots, filepath.Join(h.Path, "AppData", "Roaming", "ai.opencode.desktop"))
		case crossmount.OSDarwin:
			roots = append(roots, filepath.Join(h.Path, "Library", "Application Support", "ai.opencode.desktop"))
		case crossmount.OSLinux:
			roots = append(roots, filepath.Join(h.Path, ".config", "ai.opencode.desktop"))
		}
	}
	return roots
}

// tableExists returns true when the given table is present in the
// SQLite database. Used by loadTodoEvents (and similar future
// optional-table queries) to degrade gracefully on OpenCode schemas
// that don't have every table.
func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
