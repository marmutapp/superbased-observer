package provenance

import "testing"

func TestActorTypeRoundTrip(t *testing.T) {
	t.Parallel()

	for _, a := range AllActorTypes {
		a := a
		t.Run(string(a), func(t *testing.T) {
			t.Parallel()

			if !a.Valid() {
				t.Fatalf("AllActorTypes member %q reports !Valid()", a)
			}
			if got := a.String(); got != string(a) {
				t.Errorf("String() = %q, want %q", got, string(a))
			}
			parsed, err := Parse(a.String())
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", a.String(), err)
			}
			if parsed != a {
				t.Errorf("Parse(%q) = %q, want %q", a.String(), parsed, a)
			}
		})
	}
}

func TestTelemetryValue(t *testing.T) {
	t.Parallel()

	for _, a := range AllActorTypes {
		want := string(a)
		if a == ActorSystem {
			want = "system_agent"
		}
		if got := a.TelemetryValue(); got != want {
			t.Errorf("%q.TelemetryValue() = %q, want %q", a, got, want)
		}
	}
	// Explicit: the load-bearing mapping.
	if got := ActorSystem.TelemetryValue(); got != "system_agent" {
		t.Errorf("ActorSystem.TelemetryValue() = %q, want %q", got, "system_agent")
	}
	if got := ActorHuman.TelemetryValue(); got != "human" {
		t.Errorf("ActorHuman.TelemetryValue() = %q, want %q", got, "human")
	}
}

func TestParseUnknown(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "System", "human ", "unknown", "system_agent"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", s)
		}
	}
}

func TestValidRejectsZeroAndUnknown(t *testing.T) {
	t.Parallel()

	for _, a := range []ActorType{"", "system_agent", "robot"} {
		if a.Valid() {
			t.Errorf("%q.Valid() = true, want false", a)
		}
	}
}

func TestAllActorTypesComplete(t *testing.T) {
	t.Parallel()

	if len(AllActorTypes) != 5 {
		t.Fatalf("AllActorTypes has %d members, want 5", len(AllActorTypes))
	}
	seen := map[ActorType]int{}
	for _, a := range AllActorTypes {
		seen[a]++
	}
	for _, a := range []ActorType{ActorHuman, ActorCodingAgent, ActorPolicyAgent, ActorInsightAgent, ActorSystem} {
		if seen[a] != 1 {
			t.Errorf("AllActorTypes contains %q %d times, want exactly 1", a, seen[a])
		}
	}
}
