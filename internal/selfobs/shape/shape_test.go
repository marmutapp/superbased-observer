package shape

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/marmutapp/superbased-observer/internal/provenance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
)

func collect(kvs []attribute.KeyValue) map[string]attribute.Value {
	m := make(map[string]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// TestFixedProducer proves keys.ActorType is always "system_agent" and is NOT
// derived from InitiatedBy (Mut D target).
func TestFixedProducer(t *testing.T) {
	t.Parallel()

	r := run.DecisionRun{RunID: "r1", InitiatedBy: provenance.ActorHuman}
	m := collect(Attributes(r))

	if got := m["sbo.actor.type"].AsString(); got != "system_agent" {
		t.Errorf("sbo.actor.type = %q, want %q", got, "system_agent")
	}
	if got := m["sbo.initiated_by"].AsString(); got != "human" {
		t.Errorf("sbo.initiated_by = %q, want %q", got, "human")
	}
}

func TestOmitWhenEmpty(t *testing.T) {
	t.Parallel()

	m := collect(Attributes(run.DecisionRun{RunID: "r1"}))
	for _, k := range []string{"sbo.trigger", "sbo.component", "sbo.outcome", keyGenAIModel, keyGenAISystem, keyGenAIInputToks, keyToolAccesses, keyDecisions} {
		if _, ok := m[k]; ok {
			t.Errorf("expected %q to be omitted on an empty run", k)
		}
	}
	// LatencyMS and the four provenance scalars are always present.
	for _, k := range []string{"sbo.actor.type", "sbo.initiated_by", "sbo.run.id", "sbo.run.trace_id", "sbo.latency_ms"} {
		if _, ok := m[k]; !ok {
			t.Errorf("expected %q to always be present", k)
		}
	}
}

func TestFullyPopulated(t *testing.T) {
	t.Parallel()

	r := run.DecisionRun{
		RunID: "r1", TraceID: "t1", Trigger: "cron", Component: "routing",
		Model: "opus", Provider: "anthropic",
		LatencyMS: 12, InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		CostUSD: 0.25, Outcome: "accepted", InitiatedBy: provenance.ActorHuman,
		ToolAccesses: []string{"read", "write"}, Decisions: []string{"switch"},
	}
	m := collect(Attributes(r))

	if got := m[keyGenAIModel].AsString(); got != "opus" {
		t.Errorf("model = %q", got)
	}
	if got := m[keyGenAISystem].AsString(); got != "anthropic" {
		t.Errorf("system = %q", got)
	}
	if got := m[keyGenAIInputToks].AsInt64(); got != 100 {
		t.Errorf("input = %d", got)
	}
	if got := m[keyGenAITotalToks].AsInt64(); got != 150 {
		t.Errorf("total = %d", got)
	}
	if got := m["sbo.cost.usd"].AsFloat64(); got != 0.25 {
		t.Errorf("cost = %v", got)
	}
	if got := m[keyToolAccesses].AsStringSlice(); len(got) != 2 {
		t.Errorf("tool_accesses = %v", got)
	}
}

func TestScalarBound(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", maxScalarValueLen+500)
	m := collect(Attributes(run.DecisionRun{RunID: long}))
	got := m["sbo.run.id"].AsString()
	if len(got) > maxScalarValueLen {
		t.Errorf("scalar not capped: len = %d, want <= %d", len(got), maxScalarValueLen)
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped scalar is not valid UTF-8")
	}
}

func TestScalarBoundRuneBoundary(t *testing.T) {
	t.Parallel()

	// "€" is 3 bytes. Fill so the byte cap lands mid-rune.
	euro := "€"
	s := strings.Repeat(euro, (maxScalarValueLen/len(euro))+10)
	m := collect(Attributes(run.DecisionRun{Component: s}))
	got := m["sbo.component"].AsString()
	if len(got) > maxScalarValueLen {
		t.Errorf("len = %d, want <= %d", len(got), maxScalarValueLen)
	}
	if !utf8.ValidString(got) {
		t.Errorf("multi-byte scalar split mid-rune (invalid UTF-8)")
	}
}

func TestArrayElemCountBound(t *testing.T) {
	t.Parallel()

	in := make([]string, maxArrayElems+10)
	for i := range in {
		in[i] = "x"
	}
	m := collect(Attributes(run.DecisionRun{Decisions: in}))
	got := m[keyDecisions].AsStringSlice()
	if len(got) != maxArrayElems {
		t.Errorf("array elem count = %d, want %d", len(got), maxArrayElems)
	}
}

func TestArrayElemLenBound(t *testing.T) {
	t.Parallel()

	euro := "€" // 3 bytes; force mid-rune cut at maxArrayElemLen.
	long := strings.Repeat(euro, (maxArrayElemLen/len(euro))+10)
	m := collect(Attributes(run.DecisionRun{ToolAccesses: []string{long}}))
	got := m[keyToolAccesses].AsStringSlice()
	if len(got) != 1 {
		t.Fatalf("array len = %d, want 1", len(got))
	}
	if len(got[0]) > maxArrayElemLen {
		t.Errorf("array elem not capped: len = %d, want <= %d", len(got[0]), maxArrayElemLen)
	}
	if !utf8.ValidString(got[0]) {
		t.Errorf("array elem split mid-rune (invalid UTF-8)")
	}
}
