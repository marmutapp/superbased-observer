package run

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/provenance"
)

func TestProducerIsFixedSystem(t *testing.T) {
	t.Parallel()

	// The producer is fixed regardless of InitiatedBy.
	for _, ib := range provenance.AllActorTypes {
		r := DecisionRun{InitiatedBy: ib}
		if r.Producer() != provenance.ActorSystem {
			t.Errorf("Producer() = %q with InitiatedBy=%q, want %q", r.Producer(), ib, provenance.ActorSystem)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		run     DecisionRun
		wantErr bool
	}{
		{"empty InitiatedBy ok", DecisionRun{RunID: "r1"}, false},
		{"human initiated ok", DecisionRun{RunID: "r1", InitiatedBy: provenance.ActorHuman}, false},
		{"system initiated ok", DecisionRun{RunID: "r1", InitiatedBy: provenance.ActorSystem}, false},
		{"invalid InitiatedBy", DecisionRun{RunID: "r1", InitiatedBy: provenance.ActorType("robot")}, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run.Validate()
			if tc.wantErr != (err != nil) {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
