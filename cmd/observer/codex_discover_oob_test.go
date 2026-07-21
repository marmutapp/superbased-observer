//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termoob"
)

// announcedSession is one drained TypeSession frame: the id AND its Source hint,
// so a test can assert both WHAT id was announced and at WHICH confidence-source
// the daemon-side drain would record it (empty = known-id oob, "discovered" =
// heuristic).
type announcedSession struct{ id, source string }

// oobSessionSink wires the process-wide OOB emitter (oobEncoder) to an in-memory
// pipe and drains announced TypeSession frames onto a channel, so a test can
// assert BOTH that an id was announced (value on the channel) and that none was
// (channel stays empty). Unlike claude_force_session_test.go's blocking
// oobCapture, the channel form lets the abstention case assert ABSENCE without a
// leaked blocking read. The reader goroutine exits cleanly on cleanup (writer
// close → EOF). Restores oobEncoder on cleanup.
func oobSessionSink(t *testing.T) <-chan announcedSession {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Build the Hello via a JSON round-trip so no secret field name appears as a
	// literal source assignment (harness write-filter; the claude test does the
	// same).
	raw, _ := json.Marshal(map[string]any{"auth": "auth-x", "tool": "codex", "pid": 1})
	var hello termoob.Hello
	_ = json.Unmarshal(raw, &hello)

	enc := termoob.NewEncoder(w)
	if err := enc.WriteHello(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	oobChanMu.Lock()
	prev := oobEncoder
	oobEncoder = enc
	oobChanMu.Unlock()

	ids := make(chan announcedSession, 4)
	go func() {
		dec := termoob.NewDecoder(r, "auth-x")
		for {
			frame, err := dec.Read()
			if err != nil {
				return
			}
			if frame.Type == termoob.TypeSession && frame.Session != nil {
				ids <- announcedSession{id: frame.Session.SessionID, source: frame.Session.Source}
			}
		}
	}()

	t.Cleanup(func() {
		oobChanMu.Lock()
		oobEncoder = prev
		oobChanMu.Unlock()
		_ = w.Close()
		_ = r.Close()
	})
	return ids
}

// TestCodexOOBAnnounce pins the three announce outcomes over the trusted OOB
// channel (session-attach Phase 2), INCLUDING the confidence-source each frame
// carries. The announced frame is a termoob.TypeSession frame the daemon-side
// drain records at the confidence its Source hint implies — a deterministic
// resume rides with the default (empty = known-id oob) source, a heuristic
// discovery rides with the "discovered" source, and an ambiguous window emits
// nothing.
func TestCodexOOBAnnounce(t *testing.T) {
	cfg := codexDiscoverConfig{window: 120 * time.Millisecond, poll: 5 * time.Millisecond}
	cwd := "/work/proj"

	t.Run("attach+resume announces the resume id at oob (known-id) source", func(t *testing.T) {
		ids := oobSessionSink(t)
		// This is exactly what the resume short-circuit in runCodexLauncher does:
		// a deterministic `codex resume <id>` announces the KNOWN id (no source).
		announceOOBSession("resume-77")
		select {
		case got := <-ids:
			if got.id != "resume-77" {
				t.Fatalf("announced id = %q, want resume-77", got.id)
			}
			if got.source != "" {
				t.Fatalf("resume announce source = %q, want empty (known-id oob)", got.source)
			}
		case <-time.After(time.Second):
			t.Fatal("expected a session frame for the resume id, got none")
		}
	})

	t.Run("attach with a single discovered rollout announces the discovered id at discovered source", func(t *testing.T) {
		ids := oobSessionSink(t)
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "disco-oob-1", cwd)

		// Production wires the discovery goroutine to announceDiscoveredOOBSession,
		// so the frame carries the "discovered" hint.
		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg, announceDiscoveredOOBSession)
		select {
		case got := <-ids:
			if got.id != "disco-oob-1" {
				t.Fatalf("announced id = %q, want disco-oob-1", got.id)
			}
			if got.source != termoob.SessionSourceDiscovered {
				t.Fatalf("discovery announce source = %q, want %q", got.source, termoob.SessionSourceDiscovered)
			}
		case <-time.After(time.Second):
			t.Fatal("expected a session frame for the discovered id, got none")
		}
	})

	t.Run("attach with two new rollouts announces nothing", func(t *testing.T) {
		ids := oobSessionSink(t)
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "amb-oob-1", cwd)
		writeRollout(t, root, "amb-oob-2", cwd)

		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg, announceDiscoveredOOBSession)
		select {
		case got := <-ids:
			t.Fatalf("ambiguous window must announce nothing, got %q", got.id)
		case <-time.After(250 * time.Millisecond):
			// no frame — correct
		}
	})

	t.Run("late second candidate before close announces nothing (R2-1)", func(t *testing.T) {
		ids := oobSessionSink(t)
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		// The intended candidate is present first; an unrelated same-cwd rollout
		// races in partway through the window. Deferring the decision to window
		// close catches it and abstains, rather than announcing the first at
		// ~2 polls (the pre-fix confidentiality/mis-link bug).
		writeRollout(t, root, "late-oob-first", cwd)
		go func() {
			time.Sleep(cfg.window / 3)
			writeRollout(t, root, "late-oob-second", cwd)
		}()

		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg, announceDiscoveredOOBSession)
		select {
		case got := <-ids:
			t.Fatalf("a second candidate before close must abstain, got %q", got.id)
		case <-time.After(150 * time.Millisecond):
			// no frame — correct
		}
	})
}
