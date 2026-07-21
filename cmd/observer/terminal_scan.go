package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termscan"
)

// terminal_scan.go wires the UNTRUSTED OSC hint parser (internal/termscan, F3)
// to the always-on PTY output tap (termsession Options.OnOutput). It owns one
// bounded scanner per live PTY handle, turns parsed hints into TrustHint status
// events (internal/termfeed, for F4) and durable command-finished boundaries
// (terminal_commands, trust='hint'). Everything here treats terminal bytes as
// attacker-controlled: the parser is bounded/fuzzed, hints never authorize, and
// no command text or output is ever stored — only boundary coordinates.

// terminalScanHub owns per-handle scanners. It is safe for concurrent use; the
// pump feeds one handle from a single goroutine, but Drop/Observe for different
// handles run concurrently.
type terminalScanHub struct {
	feed     *termfeed.Feed
	st       *store.Store
	runIDFor func(handle string) (string, bool)
	logger   *slog.Logger

	mu       sync.Mutex
	scanners map[string]*handleScan
}

type handleScan struct {
	sc      *termscan.Scanner
	turnSeq int
}

func newTerminalScanHub(feed *termfeed.Feed, st *store.Store, runIDFor func(string) (string, bool), logger *slog.Logger) *terminalScanHub {
	return &terminalScanHub{
		feed: feed, st: st, runIDFor: runIDFor, logger: logger,
		scanners: make(map[string]*handleScan),
	}
}

// Observe is wired to termsession Options.OnOutput. It feeds the handle's
// scanner synchronously (bounded, cheap) and never blocks the pump.
func (h *terminalScanHub) Observe(handle string, p []byte) {
	h.scannerFor(handle).sc.Write(p)
}

func (h *terminalScanHub) scannerFor(handle string) *handleScan {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hs, ok := h.scanners[handle]; ok {
		return hs
	}
	hs := &handleScan{}
	hs.sc = termscan.New(func(hint termscan.Hint) { h.onHint(handle, hs, hint) })
	h.scanners[handle] = hs
	return hs
}

// onHint publishes each hint as a TrustHint event and persists a command
// boundary on command-finished. Store writes go on a goroutine so the pump is
// never blocked by disk I/O.
func (h *terminalScanHub) onHint(handle string, hs *handleScan, hint termscan.Hint) {
	runID, _ := h.runIDFor(handle)
	if h.feed != nil {
		h.feed.Publish(termfeed.Event{
			Kind:  "pty:" + string(hint.Kind),
			RunID: runID,
			Trust: termfeed.TrustHint,
			At:    time.Now().UTC(),
		})
	}
	if hint.Kind != termscan.HintCommandFinished || runID == "" || h.st == nil {
		return
	}
	h.mu.Lock()
	hs.turnSeq++
	seq := hs.turnSeq
	h.mu.Unlock()
	row := store.TerminalCommand{RunID: runID, TurnSeq: seq, EndedAt: time.Now().UTC(), Trust: "hint"}
	row.ExitCode = hint.ExitCode
	go func() {
		if err := h.st.InsertTerminalCommand(context.Background(), row); err != nil && h.logger != nil {
			h.logger.Debug("terminal scan: insert command boundary failed", "err", err, "run", runID)
		}
	}()
}

// Drop forgets a handle's scanner (called on session exit).
func (h *terminalScanHub) Drop(handle string) {
	h.mu.Lock()
	delete(h.scanners, handle)
	h.mu.Unlock()
}
