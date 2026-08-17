package run

import (
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/provenance"
)

// DecisionRun describes one platform decision run for self-observability. The
// run is ALWAYS a system_agent producer (Producer is fixed to
// provenance.ActorSystem); InitiatedBy names the actor that initiated it.
type DecisionRun struct {
	// RunID is the decision-run id.
	RunID string
	// TraceID is a persisted CORRELATION id (surfaced as sbo.run.trace_id),
	// NOT the OTel trace id.
	TraceID string
	// Trigger is cron|webhook|manual|anomaly.
	Trigger string
	// Component is the emitting decision component name (used as the span name).
	Component string
	// Model is the model used (empty for a pure-rule run).
	Model string
	// Provider is the provider/system (empty for a pure-rule run).
	Provider string
	// ToolAccesses is a bounded list of tool accesses (content-risk; capped by
	// the shaper).
	ToolAccesses []string
	// LatencyMS is the run latency in milliseconds.
	LatencyMS int64
	// InputTokens/OutputTokens/TotalTokens are the run's token usage.
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	// CostUSD is the run's cost.
	CostUSD float64
	// Decisions is a bounded list of decision strings (content-risk; capped by
	// the shaper).
	Decisions []string
	// ErrMsg is the error message when the run failed (sets span error status).
	ErrMsg string
	// Outcome is accepted|rejected|dismissed|verified|"".
	Outcome string
	// InitiatedBy is the actor that initiated the run (separate from the fixed
	// system_agent producer).
	InitiatedBy provenance.ActorType
}

// Producer returns the fixed producer actor type for every decision run: a
// platform run is always a system agent.
func (r DecisionRun) Producer() provenance.ActorType { return provenance.ActorSystem }

// Validate asserts the run is well-formed for emission: the producer must be
// the fixed system agent, and InitiatedBy (when set) must be a valid actor type.
func (r DecisionRun) Validate() error {
	if r.Producer() != provenance.ActorSystem {
		return fmt.Errorf("selfobs/run: producer must be %q, got %q", provenance.ActorSystem, r.Producer())
	}
	if r.InitiatedBy != "" && !r.InitiatedBy.Valid() {
		return fmt.Errorf("selfobs/run: invalid InitiatedBy actor type %q", r.InitiatedBy)
	}
	return nil
}
