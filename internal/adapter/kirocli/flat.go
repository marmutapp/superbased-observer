package kirocli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// flatState is the `<uuid>.json` session-state envelope. Both observed
// shapes decode into it: the finished shape (user_turn_metadatas
// populated) and the live/killed shape (user_turn_metadatas absent →
// nil slice, no token events).
type flatState struct {
	SessionID    string           `json:"session_id"`
	CWD          string           `json:"cwd"`
	CreatedAt    string           `json:"created_at"`
	UpdatedAt    string           `json:"updated_at"`
	Title        string           `json:"title"`
	SessionState flatSessionState `json:"session_state"`
}

type flatSessionState struct {
	ConversationMetadata flatConvMeta `json:"conversation_metadata"`
	RTSModelState        flatRTSModel `json:"rts_model_state"`
}

type flatRTSModel struct {
	ModelInfo struct {
		ModelID string `json:"model_id"`
	} `json:"model_info"`
}

type flatConvMeta struct {
	UserTurnMetadatas []flatTurnMeta `json:"user_turn_metadatas"`
}

type flatTurnMeta struct {
	MessageIDs             []string       `json:"message_ids"`
	InputTokenCount        int64          `json:"input_token_count"`
	OutputTokenCount       int64          `json:"output_token_count"`
	ContextUsagePercentage float64        `json:"context_usage_percentage"`
	MeteringUsage          []flatMetering `json:"metering_usage"`
	EndReason              string         `json:"end_reason"`
	EndTimestamp           string         `json:"end_timestamp"`
}

type flatMetering struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// flatStreamLine is one line of the `<uuid>.jsonl` message stream.
type flatStreamLine struct {
	Version string        `json:"version"`
	Kind    string        `json:"kind"`
	Data    flatStreamMsg `json:"data"`
}

type flatStreamMsg struct {
	MessageID string            `json:"message_id"`
	Content   []flatStreamBlock `json:"content"`
	Meta      struct {
		Timestamp int64 `json:"timestamp"`
	} `json:"meta"`
}

type flatStreamBlock struct {
	Kind string `json:"kind"`
	Data string `json:"data"`
}

// parseFlatBundle parses an interactive flat-file session bundle. Both
// the `.json` and `.jsonl` triggers route here; events are always
// emitted under the canonical `.jsonl` SourceFile so the store's
// (source_file, source_event_id) dedup drops the cross-trigger
// duplicates. NewOffset is the size of the TRIGGERING file so each
// per-path parse cursor advances on its own file's growth; the parse
// itself is idempotent (deterministic, content-derived SourceEventIDs)
// so a full re-read every tick is safe.
func (a *Adapter) parseFlatBundle(ctx context.Context, trigger string, fromOffset int64) (adapter.ParseResult, error) {
	res := adapter.ParseResult{NewOffset: fromOffset}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if fi, err := os.Stat(trigger); err == nil {
		res.NewOffset = fi.Size()
	}

	jsonlPath, jsonPath, sessionID := bundlePaths(trigger)

	// Read the sibling `.json` state (best-effort — a live session's
	// stream may exist before the state flush). The canonical session id
	// is the FILENAME uuid (kiro names the bundle <session_id>.json), so
	// the embedded session_id is NOT allowed to override it (§4.5a — a
	// re-keyed id orphans rows). In practice they are always equal.
	state, turnByMsg := readFlatState(jsonPath)
	projectRoot, gitBranch := resolveProjectRoot(state.CWD)
	model := state.SessionState.RTSModelState.ModelInfo.ModelID

	body, err := os.ReadFile(jsonlPath) //nolint:gosec // jsonlPath derives from a validated watch-root trigger
	if err != nil {
		if os.IsNotExist(err) {
			// The `.json` may fire before the `.jsonl` lands; nothing to
			// emit yet.
			return res, nil
		}
		return res, nil
	}

	turnIndex := -1
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var sl flatStreamLine
		if err := json.Unmarshal([]byte(line), &sl); err != nil {
			warnf(&res, "kirocli: flat bundle %s: malformed stream line: %v", sessionID, err)
			continue
		}
		switch sl.Kind {
		case "Prompt":
			turnIndex++
			ts := unixSeconds(sl.Data.Meta.Timestamp)
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    jsonlPath,
				SourceEventID: sl.Data.MessageID + ":prompt",
				SessionID:     sessionID,
				ProjectRoot:   projectRoot,
				GitBranch:     gitBranch,
				Timestamp:     ts,
				TurnIndex:     max0(turnIndex),
				Model:         model,
				Tool:          models.ToolKiroCLI,
				ActionType:    models.ActionUserPrompt,
				Target:        a.scrubber.String(flatText(sl.Data.Content)),
				Success:       true,
				MessageID:     sl.Data.MessageID,
			})
		case "AssistantMessage":
			text := flatText(sl.Data.Content)
			meta, hasMeta := turnByMsg[sl.Data.MessageID]
			ts := time.Time{}
			if hasMeta {
				ts = parseRFC3339(meta.EndTimestamp)
			}
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    jsonlPath,
				SourceEventID: sl.Data.MessageID + ":assistant",
				SessionID:     sessionID,
				ProjectRoot:   projectRoot,
				GitBranch:     gitBranch,
				Timestamp:     ts,
				TurnIndex:     max0(turnIndex),
				Model:         model,
				Tool:          models.ToolKiroCLI,
				ActionType:    models.ActionAssistantMessage,
				Target:        a.scrubber.String(contentcap.Cap(text, contentcap.DefaultMaxBytes)),
				Success:       true,
				MessageID:     sl.Data.MessageID,
			})
			// Token event — emitted only when the turn accounting block
			// exists for this assistant message. The counts are honest
			// (0 when kiro reported 0); no proxy tier is possible
			// (SigV4). Credits are NOT tokens and are deliberately
			// dropped. Reliability is "unreliable": the local counts
			// were observed structurally zero.
			if hasMeta {
				res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
					SourceFile:    jsonlPath,
					SourceEventID: sl.Data.MessageID + ":tok",
					SessionID:     sessionID,
					ProjectRoot:   projectRoot,
					GitBranch:     gitBranch,
					Timestamp:     ts,
					Tool:          models.ToolKiroCLI,
					Model:         model,
					InputTokens:   meta.InputTokenCount,
					OutputTokens:  meta.OutputTokenCount,
					Source:        "jsonl",
					Reliability:   "unreliable",
					MessageID:     sl.Data.MessageID,
				})
			}
		default:
			warnf(&res, "kirocli: flat bundle %s: unknown stream kind %q", sessionID, sl.Kind)
		}
	}
	return res, nil
}

// readFlatState reads and decodes the `.json` sibling, returning the
// state plus a map from every message id in a turn's message_ids to
// that turn's accounting block. Best-effort: a missing/malformed file
// yields a zero state and a nil map (no token events).
func readFlatState(jsonPath string) (flatState, map[string]flatTurnMeta) {
	var state flatState
	body, err := os.ReadFile(jsonPath) //nolint:gosec // jsonPath derives from a validated watch-root trigger
	if err != nil {
		return state, nil
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return flatState{}, nil
	}
	turns := state.SessionState.ConversationMetadata.UserTurnMetadatas
	if len(turns) == 0 {
		return state, nil
	}
	byMsg := make(map[string]flatTurnMeta, len(turns)*2)
	for _, t := range turns {
		for _, id := range t.MessageIDs {
			byMsg[id] = t
		}
	}
	return state, byMsg
}

// flatText joins the text blocks of a stream message.
func flatText(blocks []flatStreamBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Kind == "text" {
			sb.WriteString(b.Data)
		}
	}
	return sb.String()
}

func unixSeconds(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
