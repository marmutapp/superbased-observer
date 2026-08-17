package invariant

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/marmutapp/superbased-observer/internal/provenance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/conformance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/emit"
	"github.com/marmutapp/superbased-observer/internal/selfobs/keys"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
	"github.com/marmutapp/superbased-observer/internal/selfobs/shape"
)

// captureSink is a capturing fake emit.Sink: it records every DecisionRun passed
// to Emit. ForceFlush/Shutdown are no-ops.
type captureSink struct {
	runs []run.DecisionRun
}

func (c *captureSink) Emit(_ context.Context, r run.DecisionRun) { c.runs = append(c.runs, r) }
func (c *captureSink) ForceFlush(context.Context) error          { return nil }
func (c *captureSink) Shutdown(context.Context) error            { return nil }

// compile-time assertion that captureSink satisfies the emit.Sink contract.
var _ emit.Sink = (*captureSink)(nil)

// attrString returns the string value of the attribute named key, and whether it
// was present.
func attrString(attrs []attribute.KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestDecisionComponentRunEmitsAttributableTelemetry pins that every Wired
// decision component in the conformance registry emits ATTRIBUTABLE run
// telemetry: invoking its Conformer against a capturing fake sink yields at
// least one run whose shaped attributes carry a system_agent producer, a
// non-empty initiating actor, and a non-empty run id. Wired:false components are
// tracked-not-failed (P1-10 = PARTIAL), so they are skipped here (membership is
// pinned separately by TestDecisionComponentRegistryMembership).
//
// NAMED MUTATION (do NOT leave applied): deleting the sink.Emit(...) call in the
// synthetic Conformer (while it stays registered Wired:true) MUST fail this test
// with 'registered decision component "synthetic-reference" produced no
// attributable telemetry' — the capturing sink records zero runs.
func TestDecisionComponentRunEmitsAttributableTelemetry(t *testing.T) {
	t.Parallel()

	wiredSeen := 0
	for _, c := range conformance.RegisteredComponents {
		if !c.Wired {
			continue // tracked-not-failed
		}
		wiredSeen++
		if c.Conformer == nil {
			t.Errorf("Wired decision component %q has a nil Conformer", c.Name)
			continue
		}
		sink := &captureSink{}
		c.Conformer(context.Background(), sink)
		if len(sink.runs) == 0 {
			t.Errorf("registered decision component %q produced no attributable telemetry", c.Name)
			continue
		}
		attrs := shape.Attributes(sink.runs[0])

		wantActor := provenance.ActorSystem.TelemetryValue() // "system_agent"
		if got, ok := attrString(attrs, keys.ActorType); !ok || got != wantActor {
			t.Errorf("component %q: %s = %q (present=%v), want %q", c.Name, keys.ActorType, got, ok, wantActor)
		}
		if got, ok := attrString(attrs, keys.InitiatedBy); !ok || got == "" {
			t.Errorf("component %q: %s empty/missing (present=%v)", c.Name, keys.InitiatedBy, ok)
		}
		if got, ok := attrString(attrs, keys.RunID); !ok || got == "" {
			t.Errorf("component %q: %s empty/missing (present=%v)", c.Name, keys.RunID, ok)
		}
	}

	if wiredSeen == 0 {
		t.Fatal("no Wired decision component in conformance.RegisteredComponents — the emission contract is unproven")
	}
}

// TestDecisionComponentRegistryMembership pins the registry SHAPE independently
// of the emission check, so a mutation cannot pass by de-registering a component
// or flipping the synthetic entry's Wired bool: the six required component names
// must all be present, and the synthetic reference must be Wired:true with a
// non-nil Conformer.
func TestDecisionComponentRegistryMembership(t *testing.T) {
	t.Parallel()

	byName := make(map[string]conformance.Component, len(conformance.RegisteredComponents))
	for _, c := range conformance.RegisteredComponents {
		byName[c.Name] = c
	}

	required := []string{
		conformance.ComponentRouting,
		conformance.ComponentAdvisor,
		conformance.ComponentAdmission,
		conformance.ComponentEval,
		conformance.ComponentInsightAgent,
		conformance.ComponentSyntheticReference,
	}
	for _, name := range required {
		if _, ok := byName[name]; !ok {
			t.Errorf("required decision component %q missing from RegisteredComponents", name)
		}
	}

	syn, ok := byName[conformance.ComponentSyntheticReference]
	if !ok {
		t.Fatalf("synthetic reference conformer %q not registered", conformance.ComponentSyntheticReference)
	}
	if !syn.Wired {
		t.Errorf("synthetic reference conformer must be registered Wired:true, got Wired:false")
	}
	if syn.Conformer == nil {
		t.Errorf("synthetic reference conformer must have a non-nil Conformer")
	}
}
