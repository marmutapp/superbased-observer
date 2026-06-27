package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
)

// TestStreamProcessBridgeEmitsHelloAndEvents drives the capturer's stream
// contract against the real (local) process table: a hello frame first, then
// at least one event from the initial snapshot, all valid NDJSON. Cross-OS —
// exercises ToolHelp+PEB on the Windows host and /proc under WSL.
func TestStreamProcessBridgeEmitsHelloAndEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	if err := streamProcessBridge(ctx, &buf, 300*time.Millisecond); err != nil {
		t.Fatalf("streamProcessBridge: %v", err)
	}

	dec := bridge.NewDecoder(&buf)
	first, err := dec.Next()
	if err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if first.Kind != bridge.KindHello {
		t.Fatalf("first frame kind = %q, want hello", first.Kind)
	}
	if first.V != bridge.WireVersion {
		t.Fatalf("hello wire version = %d, want %d", first.V, bridge.WireVersion)
	}
	if first.Hello == nil || first.Hello.OS == "" || first.Hello.Backend != "poll" {
		t.Fatalf("hello frame malformed: %+v", first.Hello)
	}

	events := 0
	for {
		f, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if f.Kind == bridge.KindEvent {
			if f.Event == nil {
				t.Fatal("event frame has nil Event")
			}
			events++
		}
	}
	if events == 0 {
		t.Fatal("expected at least one process event from the initial snapshot")
	}
}
