package main

import "github.com/marmutapp/superbased-observer/internal/routing"

// handoffTargetShapes maps a canonical target-tool name to the provider
// wire shape its default models speak. This is DATA, not control flow
// (CLAUDE.md rule #5): the resolver walks the table, it never switches on
// tool name. There is no tool→shape field on the integration registry
// today (routing.ShapeForModel is model→shape), so this small local table
// grounds the mapping at the cmd boundary.
//
// Only tools whose default model family is confidently grounded appear
// here: claude-code/cline/cline-cli/cursor default to Anthropic claude
// models; codex/copilot/copilot-cli to OpenAI gpt models;
// gemini-cli/antigravity to Google gemini models. Multi-provider or
// router-fronted tools (opencode, kilo-code*, hermes, cowork, openclaw,
// pi) are deliberately omitted — a router is not a quality class, and an
// absent tool honestly degrades to the source-model fallback.
var handoffTargetShapes = map[string]routing.ProviderShape{
	"claude-code":     routing.ShapeAnthropic,
	"cline":           routing.ShapeAnthropic,
	"cline-cli":       routing.ShapeAnthropic,
	"cursor":          routing.ShapeAnthropic,
	"codex":           routing.ShapeOpenAI,
	"copilot":         routing.ShapeOpenAI,
	"copilot-cli":     routing.ShapeOpenAI,
	"gemini-cli":      routing.ShapeGoogle,
	"antigravity":     routing.ShapeGoogle,
	"antigravity-cli": routing.ShapeGoogle,
}

// resolveTargetModelViaTier is the pure target-model resolver: given a
// source model and a target tool, it returns the target provider's curated
// representative of the source model's capability tier, or "" to fall back
// to the source model. It returns "" (honest fallback) when:
//   - the target tool has no grounded provider shape (absent from the table);
//   - the source and target share a provider shape (the source model is
//     already the right one — e.g. cline → claude-code, both Anthropic);
//   - the source model's tier is unknown (TierUnclassified);
//   - the (target shape, tier) cell has no curated representative.
func resolveTargetModelViaTier(table *routing.TierTable, targetShapes map[string]routing.ProviderShape, sourceModel, targetTool string) string {
	targetShape, ok := targetShapes[targetTool]
	if !ok {
		return "" // unknown target family → source-model fallback
	}
	if routing.ShapeForModel(sourceModel) == targetShape {
		return "" // same provider shape → source model is correct as-is
	}
	tier, _ := table.Lookup(sourceModel)
	if tier == routing.TierUnclassified {
		return "" // unknown source tier → honest source-model fallback
	}
	rep, ok := table.Representative(targetShape, tier)
	if !ok {
		return "" // no curated representative for (shape, tier)
	}
	return rep
}

// newHandoffTargetResolver builds the handoffsvc.Deps.ResolveTargetModel
// closure over a seed tier table. A handoff estimate is a read-only,
// off-hot-path computation, so the cheap seed resolver (no live [routing]
// overrides) is sufficient for the Lookup/Representative reads.
func newHandoffTargetResolver() func(sourceModel, targetTool string) string {
	table := routing.NewTierResolver().Table()
	return func(sourceModel, targetTool string) string {
		return resolveTargetModelViaTier(table, handoffTargetShapes, sourceModel, targetTool)
	}
}
