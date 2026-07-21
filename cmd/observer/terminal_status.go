package main

import (
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termstatus"
)

// terminal_status.go is the F4 flagship: it fuses the trusted + untrusted signal
// streams into a per-run agent status and serves it to the dashboard through the
// dashboard.TerminalStatusProvider seam. It maintains per-run signal state from
// the in-process event feed (internal/termfeed) — OSC hints (untrusted) and
// OOB/launch/exit lifecycle (trusted) — and pulls PTY output-recency + exit
// straight from termsession at classification time. Classification itself is the
// pure internal/termstatus package; this hub only collects signals and pushes
// changes (event-driven, plus a low-frequency recompute so a working→idle
// transition that happens purely by the passage of time is still emitted).
//
// The hook/transcript trusted-turn stream is NOT yet wired here (hooks run as
// separate processes writing the DB; the transcript tail lives in the watcher).
// termstatus already accepts those signals, so wiring them later is additive —
// until then status rests on PTY recency + OSC hints + OOB lifecycle, with the
// honest "unknown"/confidence surfacing covering the gap.

// statusRecomputeInterval is the internal recompute tick. It is NOT client
// polling — it lets a time-based transition (e.g. working→idle after minutes of
// silence) surface without waiting for an event.
const statusRecomputeInterval = 5 * time.Second

type runSig struct {
	promptKind termstatus.PromptKind
	promptAt   time.Time
	bellAt     time.Time
	hookKind   termstatus.HookKind
	hookAt     time.Time
}

// terminalStatusHub implements dashboard.TerminalStatusProvider.
type terminalStatusHub struct {
	feed           *termfeed.Feed
	lastActivity   func(handle string) (time.Time, bool)
	exitStatus     func(handle string) (bool, int, bool)
	runIDForHandle func(handle string) (string, bool)
	handleForRun   func(runID string) (string, bool)
	liveHandles    func() []string
	th             termstatus.Thresholds
	now            func() time.Time

	mu   sync.Mutex
	sig  map[string]*runSig                        // runID -> accumulated signals
	last map[string]dashboard.TerminalStatusResult // handle -> last broadcast (change detection)
	subs map[*statusSub]struct{}

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newTerminalStatusHub(
	feed *termfeed.Feed,
	lastActivity func(string) (time.Time, bool),
	exitStatus func(string) (bool, int, bool),
	runIDForHandle func(string) (string, bool),
	handleForRun func(string) (string, bool),
	liveHandles func() []string,
	th termstatus.Thresholds,
) *terminalStatusHub {
	h := &terminalStatusHub{
		feed: feed, lastActivity: lastActivity, exitStatus: exitStatus,
		runIDForHandle: runIDForHandle, handleForRun: handleForRun, liveHandles: liveHandles,
		th: th, now: func() time.Time { return time.Now().UTC() },
		sig:  make(map[string]*runSig),
		last: make(map[string]dashboard.TerminalStatusResult),
		subs: make(map[*statusSub]struct{}),
		stop: make(chan struct{}),
	}
	h.wg.Add(1)
	go h.run()
	return h
}

// Stop ends the run loop.
func (h *terminalStatusHub) Stop() {
	h.stopOnce.Do(func() { close(h.stop) })
	h.wg.Wait()
}

func (h *terminalStatusHub) run() {
	defer h.wg.Done()
	sub := h.feed.Subscribe()
	defer h.feed.Unsubscribe(sub)
	t := time.NewTicker(statusRecomputeInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case ev := <-sub.C():
			h.ingest(ev)
			if handle, ok := h.handleFor(ev); ok {
				h.recomputeAndBroadcast(handle)
			}
		case <-t.C:
			for _, handle := range h.liveHandles() {
				h.recomputeAndBroadcast(handle)
			}
		}
	}
}

// handleFor resolves the PTY handle an event concerns (via its run id).
func (h *terminalStatusHub) handleFor(ev termfeed.Event) (string, bool) {
	if ev.RunID == "" {
		return "", false
	}
	return h.handleForRun(ev.RunID)
}

// ingest folds a feed event into the run's signal state.
func (h *terminalStatusHub) ingest(ev termfeed.Event) {
	if ev.RunID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.sig[ev.RunID]
	if st == nil {
		st = &runSig{}
		h.sig[ev.RunID] = st
	}
	switch {
	case ev.Kind == "pty:prompt_start":
		st.promptKind, st.promptAt = termstatus.PromptStart, ev.At
	case ev.Kind == "pty:command_start":
		st.promptKind, st.promptAt = termstatus.PromptCommandStart, ev.At
	case ev.Kind == "pty:command_executed":
		st.promptKind, st.promptAt = termstatus.PromptCommandExecuted, ev.At
	case ev.Kind == "pty:command_finished":
		st.promptKind, st.promptAt = termstatus.PromptCommandFinished, ev.At
	case ev.Kind == "pty:bell":
		st.bellAt = ev.At
	case strings.HasPrefix(ev.Kind, "oob:"):
		// Trusted lifecycle presence; kept for future hook-style weighting.
		st.hookAt = ev.At
	}
}

// compute builds the fused status for a handle.
func (h *terminalStatusHub) compute(handle string) (dashboard.TerminalStatusResult, bool) {
	la, live := h.lastActivity(handle)
	runID, _ := h.runIDForHandle(handle)
	if !live && runID == "" {
		return dashboard.TerminalStatusResult{}, false
	}
	sig := termstatus.Signals{Now: h.now()}
	if live {
		sig.LastOutput = la
	}
	if exited, code, ok := h.exitStatus(handle); ok && exited {
		sig.Exited = true
		sig.ExitCode = &code
	}
	h.mu.Lock()
	if st := h.sig[runID]; st != nil {
		sig.LastPromptKind, sig.LastPromptAt = st.promptKind, st.promptAt
		sig.LastBellAt = st.bellAt
		sig.LastHookKind, sig.LastHookAt = st.hookKind, st.hookAt
	}
	h.mu.Unlock()

	res := termstatus.Classify(sig, h.th)
	return dashboard.TerminalStatusResult{
		Handle:     handle,
		RunID:      runID,
		Status:     string(res.Status),
		Evidence:   res.Evidence,
		Confidence: string(res.Confidence),
		AgeSeconds: res.AgeSeconds,
	}, true
}

// recomputeAndBroadcast computes a handle's status and broadcasts it only when
// the status or evidence changed since the last emission.
func (h *terminalStatusHub) recomputeAndBroadcast(handle string) {
	res, ok := h.compute(handle)
	if !ok {
		return
	}
	h.mu.Lock()
	prev, seen := h.last[handle]
	changed := !seen || prev.Status != res.Status || prev.Evidence != res.Evidence
	if changed {
		h.last[handle] = res
	}
	subs := make([]*statusSub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	if !changed {
		return
	}
	for _, s := range subs {
		s.offer(res)
	}
}

// StatusForHandle satisfies dashboard.TerminalStatusProvider (the point-in-time
// GET).
func (h *terminalStatusHub) StatusForHandle(handle string) (dashboard.TerminalStatusResult, bool) {
	return h.compute(handle)
}

// Subscribe satisfies dashboard.TerminalStatusProvider (the live stream). The
// new subscriber is seeded with the current status of every live handle.
func (h *terminalStatusHub) Subscribe() dashboard.TerminalStatusSubscription {
	s := &statusSub{ch: make(chan dashboard.TerminalStatusResult, 64)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	// Seed with current state (best-effort, non-blocking).
	for _, handle := range h.liveHandles() {
		if res, ok := h.compute(handle); ok {
			s.offer(res)
		}
	}
	return &statusSubHandle{hub: h, sub: s}
}

func (h *terminalStatusHub) unsubscribe(s *statusSub) {
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
	h.mu.Unlock()
}

// statusSub is one live subscriber's bounded queue (drop-oldest on overflow, so
// a slow WS client never stalls the hub).
type statusSub struct {
	ch     chan dashboard.TerminalStatusResult
	closed bool
	mu     sync.Mutex
}

func (s *statusSub) offer(r dashboard.TerminalStatusResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- r:
	default:
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- r:
		default:
		}
	}
}

// statusSubHandle adapts *statusSub to dashboard.TerminalStatusSubscription.
type statusSubHandle struct {
	hub *terminalStatusHub
	sub *statusSub
	one sync.Once
}

func (h *statusSubHandle) Updates() <-chan dashboard.TerminalStatusResult { return h.sub.ch }

func (h *statusSubHandle) Close() {
	h.one.Do(func() {
		h.sub.mu.Lock()
		h.sub.closed = true
		h.sub.mu.Unlock()
		h.hub.unsubscribe(h.sub)
	})
}
