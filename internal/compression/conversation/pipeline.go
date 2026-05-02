package conversation

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// PipelineConfig mirrors the [config.ConversationConfig] knobs the
// pipeline consults at runtime. Copied (rather than shared) so config
// reloads don't race with in-flight compression.
type PipelineConfig struct {
	// Enabled gates the whole pipeline. When false, Run is a no-op that
	// returns the original body and OriginalBytes == CompressedBytes.
	Enabled bool
	// Mode selects drop strategy: [ModeToken] or [ModeCache]. Empty
	// string defaults to [ModeToken].
	Mode string
	// TargetRatio is the fraction of the input byte count to target.
	// Defaults to 0.85 when zero.
	TargetRatio float64
	// PreserveLastN messages are pinned at score 1.0 and are never
	// dropped.
	PreserveLastN int
	// CompressTypes names the content types eligible for per-type
	// compression. An empty map means "no per-type compression" (drops
	// still run).
	CompressTypes []string
	// PrefixBytes is the byte budget used to compute the
	// message_prefix_hash. Defaults to 8KB.
	PrefixBytes int
}

// Pipeline is the top-level compression driver the proxy wires into its
// pre-forward path. Safe for concurrent use after construction.
type Pipeline struct {
	cfg      PipelineConfig
	registry *Registry
	scrubber *scrub.Scrubber
}

// NewPipeline constructs a Pipeline. Either registry or scrubber may be
// nil: a nil registry disables per-type compression; a nil scrubber
// disables the final scrub pass. Nil scrubber is only appropriate for
// tests — production callers MUST pass a real scrubber.
func NewPipeline(cfg PipelineConfig, registry *Registry, scrubber *scrub.Scrubber) *Pipeline {
	return &Pipeline{cfg: cfg, registry: registry, scrubber: scrubber}
}

// PipelineResult carries the compressed body plus per-turn counters for
// observability.
type PipelineResult struct {
	// Body is the compressed, scrubbed request body ready to forward
	// upstream. When Skipped, Body is the unmodified input.
	Body []byte
	// Skipped is true when the pipeline did nothing (disabled, non-JSON
	// body, non-Anthropic provider). Stats fields still populate
	// OriginalBytes / CompressedBytes with len(body).
	Skipped bool
	// Provider the pipeline dispatched against. Empty for non-supported
	// providers.
	Provider string
	// MessagePrefixHash is a stable SHA-256 of the first
	// [PipelineConfig.PrefixBytes] bytes of message content after
	// compression. Empty when no messages were observable.
	MessagePrefixHash string
	// OriginalBytes is len(body) before compression.
	OriginalBytes int
	// CompressedBytes is len(Body) after compression.
	CompressedBytes int
	// CompressedCount is the number of tool_result messages whose
	// bodies were rewritten by a per-type compressor.
	CompressedCount int
	// DroppedCount is the number of source messages replaced by a
	// marker.
	DroppedCount int
	// MarkerCount is the number of marker messages emitted (consecutive
	// drops collapse into one marker).
	MarkerCount int
	// MessageCount is the number of messages in the final body.
	MessageCount int
	// Events is the per-decision detail (one record per compress or
	// drop). Empty when the pipeline skipped or no decisions were made.
	// Persisted into compression_events by the store layer.
	Events []Event
}

// Run compresses body when the pipeline is enabled and the provider is
// supported. Returns the original body unchanged on any skip path so the
// proxy can always forward.
func (p *Pipeline) Run(provider string, body []byte) PipelineResult {
	result := PipelineResult{
		Body:            body,
		Skipped:         true,
		Provider:        provider,
		OriginalBytes:   len(body),
		CompressedBytes: len(body),
	}
	if !p.cfg.Enabled || len(body) == 0 {
		return result
	}
	switch provider {
	case "anthropic":
		return p.runAnthropic(provider, body, result)
	case "openai":
		return p.runOpenAI(provider, body, result)
	default:
		return result
	}
}

// runAnthropic handles the Messages API body: extract → score → enforce
// → serialize (with cache_control injection when mode=cache) → scrub.
func (p *Pipeline) runAnthropic(provider string, body []byte, skipped PipelineResult) PipelineResult {
	envelope, extracted, ok := anthropicExtract(body)
	if !ok || len(extracted) == 0 {
		return skipped
	}
	msgs := toConversationMessages(extracted)
	br, splitAt, prefixHash := p.compressMessages(msgs)

	cacheBreakpointIdx := -1
	if strings.EqualFold(p.cfg.Mode, ModeCache) && splitAt > 0 {
		cacheBreakpointIdx = splitAt - 1
	}
	newBody, err := serializeAnthropic(envelope, extracted, br.Messages, cacheBreakpointIdx)
	if err != nil {
		return skipped
	}
	return p.finalize(provider, body, newBody, br, prefixHash)
}

// runOpenAI handles Chat Completions bodies. Cache_control injection is
// not applied — Chat Completions has no standard cache marker today.
// Responses API bodies (/v1/responses) don't carry a `messages` array
// and are Skipped by openaiExtract; handling them is a future task.
func (p *Pipeline) runOpenAI(provider string, body []byte, skipped PipelineResult) PipelineResult {
	envelope, extracted, ok := openaiExtract(body)
	if !ok || len(extracted) == 0 {
		return skipped
	}
	msgs := openaiToConversationMessages(extracted)
	br, _, prefixHash := p.compressMessages(msgs)

	newBody, err := serializeOpenAI(envelope, extracted, br.Messages, -1)
	if err != nil {
		return skipped
	}
	return p.finalize(provider, body, newBody, br, prefixHash)
}

// compressMessages runs the provider-agnostic middle of the pipeline:
// score → enforce → split. The scrubber is NOT applied here — each
// caller scrubs the serialized body in [finalize].
func (p *Pipeline) compressMessages(msgs []Message) (BudgetResult, int, string) {
	scored := Score(msgs, ScoreOptions{PreserveLastN: p.cfg.PreserveLastN})
	compressAllow := buildAllow(p.cfg.CompressTypes)
	br := Enforce(scored, BudgetOptions{
		TargetRatio:   p.cfg.TargetRatio,
		Registry:      p.registry,
		CompressTypes: compressAllow,
		Mode:          p.cfg.Mode,
	})
	prefixBytes := p.cfg.PrefixBytes
	if prefixBytes <= 0 {
		prefixBytes = 8 * 1024
	}
	splitAt := SplitIndex(br.Messages, prefixBytes)
	prefixHash := PrefixHash(br.Messages, splitAt)
	return br, splitAt, prefixHash
}

// finalize scrubs the serialized body and packs the PipelineResult so
// both provider paths share the same exit shape.
func (p *Pipeline) finalize(provider string, original, newBody []byte, br BudgetResult, prefixHash string) PipelineResult {
	if p.scrubber != nil {
		newBody = []byte(p.scrubber.String(string(newBody)))
	}
	return PipelineResult{
		Body:              newBody,
		Skipped:           false,
		Provider:          provider,
		MessagePrefixHash: prefixHash,
		OriginalBytes:     len(original),
		CompressedBytes:   len(newBody),
		CompressedCount:   br.Stats.CompressedCount,
		DroppedCount:      br.Stats.DroppedCount,
		MarkerCount:       br.Stats.MarkerCount,
		MessageCount:      br.Stats.MessageCount,
		Events:            br.Stats.Events,
	}
}

// buildAllow converts a string slice of type names (from config.toml)
// into a ContentType set for the budget enforcer.
func buildAllow(names []string) map[types.ContentType]bool {
	if len(names) == 0 {
		return nil
	}
	allow := make(map[types.ContentType]bool, len(names))
	for _, n := range names {
		allow[types.ContentType(n)] = true
	}
	return allow
}
