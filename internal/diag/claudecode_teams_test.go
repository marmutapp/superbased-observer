package diag

import (
	"net"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func cfgWithIngest(enabled bool, grpc string) config.Config {
	var c config.Config
	c.Ingest.OTel.Enabled = enabled
	c.Ingest.OTel.GRPCAddr = grpc
	return c
}

func TestCheckClaudeCodeTeams_DisabledIsOK(t *testing.T) {
	got := checkClaudeCodeTeams(cfgWithIngest(false, ""))
	if got.Status != StatusOK {
		t.Fatalf("disabled should be OK, got %v: %s", got.Status, got.Message)
	}
}

func TestCheckClaudeCodeTeams_ReceiverUnreachableWarns(t *testing.T) {
	// Point at a port nothing is listening on.
	got := checkClaudeCodeTeams(cfgWithIngest(true, "127.0.0.1:1"))
	if got.Status != StatusWarn {
		t.Fatalf("unreachable receiver should WARN, got %v", got.Status)
	}
	if len(got.Details) == 0 {
		t.Fatal("expected detail bullets")
	}
}

func TestCheckClaudeCodeTeams_ReceiverReachable(t *testing.T) {
	// Stand up a throwaway listener so the dial succeeds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	got := checkClaudeCodeTeams(cfgWithIngest(true, ln.Addr().String()))
	// The receiver bullet must report reachable (overall may still WARN on the
	// absent managed-settings in CI, which is expected and fine).
	var sawReachable bool
	for _, d := range got.Details {
		if strings.Contains(d, "OTLP receiver reachable") {
			sawReachable = true
		}
	}
	if !sawReachable {
		t.Fatalf("expected a 'receiver reachable' detail, got %+v", got.Details)
	}
}

func TestDialable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if !dialable(ln.Addr().String()) {
		t.Fatal("live listener should be dialable")
	}
	if dialable("127.0.0.1:1") {
		t.Fatal("port 1 should not be dialable")
	}
}
