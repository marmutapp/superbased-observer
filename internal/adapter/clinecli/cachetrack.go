package clinecli

import (
	"encoding/json"

	"github.com/marmutapp/superbased-observer/internal/adapter/cacheobs"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// MaxBlocksPerSession caps the per-session Tier-2 accumulator's
// running block count. Matches the claudecode template's cap
// (same memory budget, same OOM guard).
const MaxBlocksPerSession = cacheobs.DefaultMaxBlocksPerSession

// buildCacheObservations walks every session's messages in order
// and emits one CacheTurnObservation per assistant message whose
// metrics block is non-nil. User-role messages contribute their
// content blocks to the running per-session chain (which feeds
// the NEXT assistant emit's prefix); assistant-role messages
// contribute their own blocks AND emit when they carry metrics.
//
// Spec §14.3 — cline-cli Tier-2. See
// docs/audits/cachetrack-cline-cli-tier2-audit-2026-06-09.md for
// the volatile-element analysis (cline-cli has NO block-level
// volatile fields — the canonical marshaller is a clean
// struct-encode per Anthropic-shape block type).
//
// Sibling to buildEvents (parse.go). Walks the SAME `sessions`
// slice but ignores all non-content concerns (session_start,
// session_end, session-level token usage, user_prompt rows,
// tool dispatch, tool_result pairing). The wiring split keeps
// buildEvents' test surface unchanged.
func buildCacheObservations(sessions []sessionRow, dbPath string) []models.CacheTurnObservation {
	var out []models.CacheTurnObservation
	for i := range sessions {
		s := &sessions[i]
		msgPath := resolveMessagesPath(s, dbPath)
		if msgPath == "" || len(s.Messages.Messages) == 0 {
			continue
		}
		acc := cacheobs.New(MaxBlocksPerSession)
		for mi := range s.Messages.Messages {
			m := &s.Messages.Messages[mi]
			blocks := accumulateCacheBlocksTier2(m.Content, m.Role)
			acc.ObserveBlocks(blocks)

			if m.Role != "assistant" || m.Metrics == nil {
				continue
			}
			model := s.Model
			provider := s.Provider
			if m.ModelInfo != nil && m.ModelInfo.ID != "" {
				model = m.ModelInfo.ID
				if m.ModelInfo.Provider != "" {
					provider = m.ModelInfo.Provider
				}
			}
			usage := models.CacheUsage{
				NetInputTokens:      m.Metrics.InputTokens,
				OutputTokens:        m.Metrics.OutputTokens,
				CacheReadTokens:     m.Metrics.CacheReadTokens,
				CacheCreationTokens: m.Metrics.CacheWriteTokens,
			}
			obs := acc.Emit(msgPath, s.ID, m.ID, model, unixMilliToTime(m.Ts), usage, false)
			// §15.3 boundary: cline-cli supports per-message
			// provider switching via m.ModelInfo. When routed to an
			// implicit-cache provider (deepseek-flash via OpenRouter
			// — the fixture-validated case in the §15.3(c) arc,
			// OpenAI, or any other table-listed implicit family),
			// overlay ImplicitCache=true. Anthropic-routed sessions
			// are unchanged.
			obs = cacheobs.ApplyImplicitCacheOverlay(obs, provider, model)
			out = append(out, obs)
		}
	}
	return out
}

// accumulateCacheBlocksTier2 converts cline-cli's Anthropic-shape
// content blocks into deterministic CacheBlockMeta entries. Per
// the audit, NO block-level volatile fields exist; the marshaller
// is a straight struct-encode per type. Each canonical payload
// uses an explicit STRUCT (NOT a map) so encoding/json's
// deterministic field order preserves the R3 byte-stability
// invariant.
func accumulateCacheBlocksTier2(blocks []messageBlock, role string) []models.CacheBlockMeta {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]models.CacheBlockMeta, 0, len(blocks))
	for _, b := range blocks {
		canon, kind, ok := marshalCanonicalBlockTier2(b)
		if !ok {
			continue
		}
		out = append(out, models.CacheBlockMeta{
			LevelLabel:     "message", // R1: transcripts never expose tools/system
			Kind:           kind,
			CanonicalBytes: canon,
			Role:           role,
		})
	}
	return out
}

// marshalCanonicalBlockTier2 produces the deterministic JSON
// canonical form of one cline-cli content block. Returns
// (canonical bytes, engine kind label, ok=false on unknown
// block type).
func marshalCanonicalBlockTier2(b messageBlock) (canon []byte, kind string, ok bool) {
	switch b.Type {
	case "text":
		payload := struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}{Type: "text", Text: b.Text}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "text", true

	case "thinking":
		// Re-map onto the engine's canonical "thinking" kind label,
		// matching the claudecode template's intent. The cline-cli
		// schema uses .thinking; the canonical bytes carry it as
		// .text so cross-adapter chain comparisons stay shape-
		// stable.
		payload := struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}{Type: "thinking", Text: b.Thinking}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "thinking", true

	case "tool_use":
		payload := struct {
			Type  string          `json:"type"`
			ID    string          `json:"id,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		}{
			Type:  "tool_use",
			ID:    b.ID,
			Name:  b.Name,
			Input: b.Input,
		}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "tool_use", true

	case "tool_result":
		payload := struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id,omitempty"`
			Name      string          `json:"name,omitempty"`
			IsError   bool            `json:"is_error,omitempty"`
			Content   json.RawMessage `json:"content,omitempty"`
		}{
			Type:      "tool_result",
			ToolUseID: b.ToolUseID,
			Name:      b.Name,
			IsError:   b.IsError,
			Content:   b.Content,
		}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "tool_result", true

	default:
		return nil, "", false
	}
}
