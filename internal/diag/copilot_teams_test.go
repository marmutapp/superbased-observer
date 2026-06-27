package diag

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func TestCheckCopilotTeams_GateOffIsOK(t *testing.T) {
	t.Setenv("COPILOT_OTEL_ENABLED", "")
	got := checkCopilotTeams(config.Config{})
	if got.Status != StatusOK {
		t.Fatalf("with no COPILOT_OTEL_ENABLED the check should be OK, got %v: %s", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "server-side") {
		t.Errorf("expected a server-side governance note, got %q", got.Message)
	}
}

func TestCheckCopilotTeams_RedirectRequestedButNoReceiverWarns(t *testing.T) {
	t.Setenv("COPILOT_OTEL_ENABLED", "true")
	// [ingest.otel].enabled defaults false → the redirect has no receiver.
	got := checkCopilotTeams(config.Config{})
	if got.Status != StatusWarn {
		t.Fatalf("redirect requested with no receiver should WARN, got %v", got.Status)
	}
	var sawNoReceiver bool
	for _, d := range got.Details {
		if strings.Contains(d, "no receiver") {
			sawNoReceiver = true
		}
	}
	if !sawNoReceiver {
		t.Fatalf("expected a 'no receiver' detail, got %+v", got.Details)
	}
}
