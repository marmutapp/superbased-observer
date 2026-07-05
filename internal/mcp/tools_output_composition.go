package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/verbosity"
)

// -----------------------------------------------------------------------------
// get_output_composition — Output Composition (Verbosity) session read tool
// (docs/plans/output-composition-verbosity-plan-2026-06-30.md).
//
// Surfaces the read-side verbosity rollup for one session over MCP so the AI
// client can ask "what's my code-vs-explanation split for this session?".
// Wraps store.LoadSessionVerbosity (visible narrative/artifact bytes segmented
// from assistant text + authored code bytes from actions.content_bytes) and
// shapes the same numbers the dashboard's verbosity card shows. Read-only;
// node-local; reports integer byte counts only, never content.
//
// Always-on (registered in builtinTools), mirroring search_symbols. MCP is
// opt-in per AI client via `observer init` (not registered by `observer start`).
// -----------------------------------------------------------------------------

type outputCompositionTool struct{ db *sql.DB }

func newOutputCompositionTool(db *sql.DB) Tool { return &outputCompositionTool{db: db} }

func (*outputCompositionTool) Name() string { return "get_output_composition" }

func (*outputCompositionTool) Description() string {
	return "Report a session's output composition: how much of the assistant's " +
		"output was code (file writes + shell commands + fenced code blocks) vs " +
		"explanation (narrative prose + prose-ish shown artifacts), by bytes, with " +
		"a code:explanation ratio, a per-category breakdown, and the code languages " +
		"that came through. Use to gauge whether a session leaned toward producing " +
		"code or prose. Read-only; node-local; byte counts only (never content). " +
		"When authored_captured is false the session has code-authoring actions " +
		"whose byte length was not measured yet — run `observer backfill`."
}

func (*outputCompositionTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "The session id to report on. Required.",
			},
		},
		"required": []string{"session_id"},
	}
}

type outputCompositionArgs struct {
	SessionID string `json:"session_id"`
}

type outputCompositionChannels struct {
	NarrativeBytes        int64 `json:"narrative_bytes"`
	ArtifactBytes         int64 `json:"artifact_bytes"`
	ArtifactUntaggedBytes int64 `json:"artifact_untagged_bytes"`
	WrittenBytes          int64 `json:"written_bytes"`
	CommandBytes          int64 `json:"command_bytes"`
}

type outputCompositionLang struct {
	Language string `json:"language"`
	Bytes    int64  `json:"bytes"`
	Category string `json:"category"`
}

type outputCompositionResult struct {
	SessionID    string  `json:"session_id"`
	TotalBytes   int64   `json:"total_bytes"`
	CodeBytes    int64   `json:"code_bytes"`
	ExplainBytes int64   `json:"explain_bytes"`
	CodePct      float64 `json:"code_pct"`
	ExplainPct   float64 `json:"explain_pct"`
	// CodeExplainRatio is code÷explanation; nil when there is no explanation
	// (the caller shows "—" rather than ∞).
	CodeExplainRatio *float64 `json:"code_explain_ratio,omitempty"`

	ByCategory     map[string]int64          `json:"by_category"`
	Channels       outputCompositionChannels `json:"channels"`
	CodeByLanguage []outputCompositionLang   `json:"code_by_language"`

	// AuthoredCaptured is false when the session HAS write/edit/command actions
	// but none carry a content_bytes measurement yet (pre-feature rows / a
	// daemon not on the content_bytes build) — the caller should prompt an
	// `observer backfill` instead of implying the session was prose-only.
	AuthoredCaptured bool `json:"authored_captured"`
}

func (t *outputCompositionTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args outputCompositionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.SessionID == "" {
		return nil, errors.New("session_id is required")
	}

	st := store.New(t.db)
	b, err := st.LoadSessionVerbosity(ctx, args.SessionID)
	if err != nil {
		return nil, err
	}
	captured, totalAuthored, err := st.AuthoredCaptureStats(ctx, args.SessionID)
	if err != nil {
		return nil, err
	}

	return buildOutputComposition(args.SessionID, b, captured, totalAuthored), nil
}

// buildOutputComposition shapes a Breakdown into the tool payload. Pure (no
// I/O) so it is unit-testable without a server; mirrors the dashboard's
// buildVerbosityResponse numbers.
func buildOutputComposition(sessionID string, b *verbosity.Breakdown, captured, totalAuthored int64) outputCompositionResult {
	cats := b.ByCategory()
	byCat := make(map[string]int64, len(cats))
	var total int64
	for c, v := range cats {
		byCat[string(c)] = v
		total += v
	}
	code := b.CodeBytes()
	explain := b.ExplainBytes()

	res := outputCompositionResult{
		SessionID:    sessionID,
		TotalBytes:   total,
		CodeBytes:    code,
		ExplainBytes: explain,
		ByCategory:   byCat,
		Channels: outputCompositionChannels{
			NarrativeBytes:        b.Visible.NarrativeBytes,
			ArtifactBytes:         b.Visible.ArtifactBytes,
			ArtifactUntaggedBytes: b.Visible.ArtifactUntaggedBytes,
			WrittenBytes:          sumInt64Map(b.Written) + sumInt64Map(b.WrittenUnknownExt),
			CommandBytes:          sumInt64Map(b.Command),
		},
		CodeByLanguage:   sortedCodeLangs(b.CodeByLang()),
		AuthoredCaptured: totalAuthored == 0 || captured > 0,
	}
	if total > 0 {
		res.CodePct = 100 * float64(code) / float64(total)
		res.ExplainPct = 100 * float64(explain) / float64(total)
	}
	if explain > 0 {
		r := float64(code) / float64(explain)
		res.CodeExplainRatio = &r
	}
	return res
}

// sortedCodeLangs turns a language→bytes map into a descending slice with the
// category attached, for a stable order.
func sortedCodeLangs(m map[string]int64) []outputCompositionLang {
	out := make([]outputCompositionLang, 0, len(m))
	for lang, by := range m {
		out = append(out, outputCompositionLang{Language: lang, Bytes: by, Category: string(verbosity.CategoryOf(lang))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Language < out[j].Language
	})
	return out
}

func sumInt64Map(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}
