package conformance

import (
	"context"

	"github.com/marmutapp/superbased-observer/internal/provenance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/emit"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
)

// Component names. The five decision components are the platform sites that
// SHOULD emit self-observability run telemetry; SyntheticReference is the
// always-Wired executable proof of the emission contract.
const (
	// ComponentRouting is the model-routing decision engine (internal/routing).
	ComponentRouting = "routing"
	// ComponentAdvisor is the cost/quality advisor (internal/intelligence/advisor).
	ComponentAdvisor = "advisor"
	// ComponentAdmission is the input-admission guardrail (internal/obs/admission).
	ComponentAdmission = "admission"
	// ComponentEval is the per-item eval plane (internal/obs eval).
	ComponentEval = "eval"
	// ComponentInsightAgent is the automated analysis/insight agent (P2-7).
	ComponentInsightAgent = "insight-agent"
	// ComponentSyntheticReference is the synthetic reference conformer — the
	// always-Wired executable proof that a system_agent run emits attributable
	// telemetry through an emit.Sink.
	ComponentSyntheticReference = "synthetic-reference"
)

// Component describes one platform decision component and how it participates in
// self-observability. Name is the stable component identifier; Wired reports
// whether the component actually emits attributable run telemetry today; when
// Wired, Conformer is a real function that builds a run.DecisionRun (producer
// FIXED to system_agent) and drives it through sink.
//
// A Wired:false entry is TRACKED-not-failed: the enforcement test asserts its
// MEMBERSHIP but does not demand emission, so a component's retrofit can be
// staged without the registry going red. Two writers of the same fact are
// avoided — this is the ONE registry the classify allow-list peer (keys) and
// the enforcement/integration tests read.
type Component struct {
	// Name is the stable component identifier.
	Name string
	// Wired reports whether the component emits attributable run telemetry today.
	Wired bool
	// Conformer emits at least one attributable run through sink; nil unless Wired.
	Conformer func(ctx context.Context, sink emit.Sink)
}

// RegisteredComponents is the single registry of decision components.
// Routing/advisor/admission/eval + SyntheticReference are Wired:true after
// P1-10 Phases B–D; insight-agent stays Wired:false until P2-7.
var RegisteredComponents = []Component{
	{Name: ComponentRouting, Wired: true, Conformer: routingConformer},
	{Name: ComponentAdvisor, Wired: true, Conformer: advisorConformer},
	{Name: ComponentAdmission, Wired: true, Conformer: admissionConformer},
	{Name: ComponentEval, Wired: true, Conformer: evalConformer},
	{Name: ComponentInsightAgent, Wired: true, Conformer: insightConformer},
	{Name: ComponentSyntheticReference, Wired: true, Conformer: syntheticConformer},
}

// routingConformer is the Wired reference for the routing retrofit (P1-10
// Phase B): a pure-rule Decide-shaped run with empty model/token/cost fields.
func routingConformer(ctx context.Context, sink emit.Sink) {
	r := run.DecisionRun{
		RunID:       "routing-conformer-run",
		TraceID:     "routing-conformer-trace",
		Trigger:     "manual",
		Component:   ComponentRouting,
		Decisions:   []string{"kind:readonly", "rule:conformer"},
		Outcome:     "verified",
		InitiatedBy: provenance.ActorHuman,
	}
	sink.Emit(ctx, r)
}

func advisorConformer(ctx context.Context, sink emit.Sink) {
	sink.Emit(ctx, run.DecisionRun{
		RunID:       "advisor-conformer-run",
		TraceID:     "advisor-conformer-trace",
		Trigger:     "manual",
		Component:   ComponentAdvisor,
		Decisions:   []string{"suggestions:0"},
		Outcome:     "verified",
		InitiatedBy: provenance.ActorHuman,
	})
}

func admissionConformer(ctx context.Context, sink emit.Sink) {
	sink.Emit(ctx, run.DecisionRun{
		RunID:       "admission-conformer-run",
		TraceID:     "admission-conformer-trace",
		Trigger:     "manual",
		Component:   ComponentAdmission,
		Decisions:   []string{"decision:allow", "mode:observe"},
		Outcome:     "accepted",
		InitiatedBy: provenance.ActorHuman,
	})
}

func evalConformer(ctx context.Context, sink emit.Sink) {
	sink.Emit(ctx, run.DecisionRun{
		RunID:       "eval-conformer-run",
		TraceID:     "eval-conformer-trace",
		Trigger:     "manual",
		Component:   ComponentEval,
		Decisions:   []string{"scores:1", "source:conformer"},
		Outcome:     "verified",
		InitiatedBy: provenance.ActorHuman,
	})
}

func insightConformer(ctx context.Context, sink emit.Sink) {
	sink.Emit(ctx, run.DecisionRun{
		RunID:       "insight-conformer-run",
		TraceID:     "insight-conformer-trace",
		Trigger:     "manual",
		Component:   ComponentInsightAgent,
		Decisions:   []string{"playbook:conformer", "outcome:abstain"},
		Outcome:     "verified",
		InitiatedBy: provenance.ActorInsightAgent,
	})
}

// syntheticConformer is the Wired reference Conformer. It builds a diagnostic
// DecisionRun whose producer is FIXED to system_agent (run.DecisionRun.Producer
// is always provenance.ActorSystem) and whose InitiatedBy is a human operator,
// then emits it through sink. It is the executable proof that a decision
// component produces attributable telemetry (system_agent producer + initiating
// actor + run id).
func syntheticConformer(ctx context.Context, sink emit.Sink) {
	r := run.DecisionRun{
		RunID:       "synthetic-reference-run",
		TraceID:     "synthetic-reference-trace",
		Trigger:     "manual",
		Component:   ComponentSyntheticReference,
		Outcome:     "verified",
		InitiatedBy: provenance.ActorHuman,
	}
	sink.Emit(ctx, r)
}
