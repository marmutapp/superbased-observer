package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
	"github.com/marmutapp/superbased-observer/internal/processobs/linuxebpf"
	"github.com/marmutapp/superbased-observer/internal/processobs/poll"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// processObserverMaxTracked caps the Attributor's live process tree as a
// leak guard. The live set is naturally bounded (exits detach), so this only
// matters if exits are somehow missed; well above any real process table.
const processObserverMaxTracked = 8192

// runProcessObserver is the daemon-resident Process Observability service
// (docs/process-observability.md §6). It is gated on
// [observer.process].enabled (opt-in, default off) and FAIL-OPEN at every
// step: a missing/unsupported backend, a privilege failure, or any runtime
// error degrades to a WARN and returns nil — it must never cancel the
// proxy/watcher/dashboard (acceptance criterion 6). Mirrors the guard-cloud /
// otel sibling goroutines: loads its own config+DB, returns nil on any miss.
func runProcessObserver(ctx context.Context, configPath string) error {
	cfg, db, cleanup, err := loadConfigAndDB(ctx, configPath)
	if err != nil {
		return nil
	}
	defer cleanup()
	if !cfg.Observer.Process.Enabled {
		return nil
	}
	logger := newLogger(cfg.Observer.LogLevel)

	backend, berr := selectProcessBackend(cfg.Observer.Process, logger)
	if berr != nil {
		logger.Warn("process observability: no backend — capture disabled (daemon continues)",
			"backend", cfg.Observer.Process.Backend, "reason", berr.Error())
		return nil
	}

	st := store.New(db)
	// Read-only seam onto the session_pid_bridge: the SessionStart hook owns
	// the writes; here we only Lookup a (root) pid → session. Descendants are
	// attributed by tree inheritance inside the Attributor, not by lookup.
	bridge := pidbridge.New(db)
	seed := func(pid int) (processobs.Seed, bool) {
		e, ok, lerr := bridge.Lookup(ctx, pid)
		if lerr != nil || !ok {
			return processobs.Seed{}, false
		}
		return processobs.Seed{
			SessionID:  e.SessionID,
			Tool:       e.Tool,
			Source:     processobs.AttrBridge,
			Confidence: processobs.ConfHigh,
		}, true
	}

	// One scrubber instance, shared by the Attributor (argv/path/env scrub at
	// exec) and the DeepEnricher (env-posture policy for the targeted environ
	// read). The DeepEnricher (poll/enrich_linux.go) is the post-attribution,
	// per-new-process seam that feeds /proc/<pid>/environ → env posture and,
	// when [observer.process.executable].hash_enabled, the exe content hash —
	// sensitive/expensive reads done once per persisted run, not whole-table
	// every poll (spec §8.1, §8 Executable, §19 Q6). It is nil on non-Linux.
	scrubber := buildProcessScrubber(cfg.Observer.Process)
	deepEnricher := poll.NewDeepEnricher(
		scrubber,
		cfg.Observer.Process.Executable.HashEnabled,
		cfg.Observer.Process.Executable.MaxHashFileSizeMB,
	)

	// A backend whose events can't be attributed at capture time (the cross-OS
	// bridge — §5.5) forces unattributed capture so the Windows rows persist
	// for the deferred CorrelateCrossOS pass to join them to a session.
	captureUnattributed := cfg.Observer.Process.CaptureUnattributed
	if uc, ok := backend.(processobs.UnattributedCapturer); ok && uc.RequiresUnattributedCapture() {
		captureUnattributed = true
		logger.Info("process observability: backend requires unattributed capture (deferred cross-OS attribution)",
			"backend", backend.Name())
	}

	// EV (§5.5 P-B6): the capturer recovers an allowlisted session-id env var
	// (e.g. CLAUDE_CODE_SESSION_ID) for new processes; this seam resolves it to
	// a session by direct equality, attributing the whole env-inheriting subtree
	// at HIGH confidence — namespace-independent, so it works across the WSL↔
	// Windows boundary where the pidbridge pid seed cannot. A miss falls back to
	// the medium CorrelateCrossOS pass.
	attributor := processobs.NewAttributor(seed, scrubber, nil)
	attributor.SetTokenLookup(func(token string) (processobs.Seed, bool) {
		s, ok, lerr := st.SessionSeedByID(ctx, token)
		if lerr != nil || !ok {
			return processobs.Seed{}, false
		}
		return s, true
	})

	// Never capture the observer daemon's OWN processes as unattributed AI
	// activity (§3.1). Include the running binary's basename (so a renamed
	// install still self-excludes) plus the canonical names, covering the
	// cross-OS bridge's Windows `observer.exe` too.
	ownBasenames := []string{"observer", "observer.exe"}
	if exe, eerr := os.Executable(); eerr == nil {
		if base := filepath.Base(exe); base != "" {
			ownBasenames = append(ownBasenames, base)
		}
	}

	obs := processobs.NewObserver(processobs.Options{
		Backend:             backend,
		Attributor:          attributor,
		DeepEnricher:        deepEnricher,
		Sink:                st,
		CaptureUnattributed: captureUnattributed,
		ExcludeOwnBasenames: ownBasenames,
		// Always capture UNATTRIBUTED AI-tool subtrees (codex/cursor/… on a
		// native host where no pidbridge seed or env-token resolves) so the
		// deferred CorrelateCrossOS cwd pass can join them to a session. This
		// is the native-Linux counterpart to the bridge's whole-table
		// unattributed capture, but bounded to AI subtrees — the rest of the
		// process table is still dropped. Gives every adapter (not just
		// Claude Code) process attribution.
		CaptureUnattributedAISubtree: true,
		BatchSize:                    cfg.Observer.Process.BatchSize,
		MaxTracked:                   processObserverMaxTracked,
	})

	// Refresh the active-session project roots that gate cwd-anchored capture
	// (extends process attribution to EVERY adapter — the generic-interpreter
	// tools like hermes-as-python / pi / roo-code / in-IDE Copilot that present
	// no branded launcher but run workers in the project dir). Seed once, then
	// on a ticker; best-effort, a query error just leaves the AI-subtree signal.
	refreshRoots := func() {
		if roots, rerr := st.ActiveSessionRoots(ctx, 60); rerr == nil {
			obs.SetActiveSessionRoots(roots)
		}
	}
	refreshRoots()
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refreshRoots()
			}
		}
	}()

	logger.Info("process observability: starting", "backend", backend.Name())
	if rerr := obs.Run(ctx); rerr != nil && !errors.Is(rerr, context.Canceled) {
		// Backend Start failure (unsupported OS / missing CAP_BPF / no /proc)
		// or a fatal runtime error — degraded, never propagated. The DB-derived
		// gauges + `observer doctor` still report persisted state.
		h := obs.Health().Snapshot()
		logger.Warn("process observability: backend stopped — capture degraded (daemon continues)",
			"backend", h.BackendName, "err", rerr)
	}
	return nil
}

// selectProcessBackend resolves [observer.process].backend to a concrete
// Backend, or an error explaining why none is available (the caller then
// fail-opens). The poll backend (§5.4) and the real-time linux_ebpf backend
// (§5.2, fail-opens to poll when uncapable) are implemented; etw /
// endpointsecurity are still stubs. "auto" PREFERS the eBPF stack when the box
// can load BPF (capability-probed; usually false for the unprivileged daemon →
// poll), composing it with poll (metric sampler + initial snapshot, dedup on
// process_key) + the bridge. Branching is on the configured capability, not host
// identity (the backend's own Start probe decides OS support and fail-opens).
func selectProcessBackend(pc config.ProcessConfig, logger *slog.Logger) (processobs.Backend, error) {
	pollMS, bridgeMS := resolveProcessPollIntervals(pc)
	pollOpts := poll.Options{Interval: time.Duration(pollMS) * time.Millisecond}
	bridgeOpts := bridge.Options{
		WindowsBinaryPath: pc.WindowsBinaryPath,
		PollIntervalMS:    bridgeMS,
		Logger:            logger,
	}
	onChildErr := func(name string, err error) {
		logger.Warn("process observability: composite child unavailable (capture continues with the rest)", "child", name, "err", err)
	}
	// newComposite runs the Linux /proc poll backend AND the Windows cross-OS
	// bridge together (fan-in), so a daemon on the canonical WSL topology
	// captures BOTH WSL-native AND Windows AI-tool processes — a single backend
	// sees only one OS's process table. Fail-open per child inside the composite.
	newComposite := func() processobs.Backend {
		return processobs.NewComposite([]processobs.Backend{
			poll.New(pollOpts),
			bridge.New(bridgeOpts),
		}, onChildErr)
	}
	// newEBPFComposite is the real-time stack: the eBPF backend (§5.2, Linux
	// lifecycle — catches the sub-poll-interval processes the poller misses) PLUS
	// the poll backend, which serves as the metric sampler + initial-snapshot
	// source (eBPF only sees execs AFTER attach; poll's first tick attributes
	// already-running roots) + the survivor metric refresh eBPF can't tick. The
	// two share a boot-id namespace, so a process seen by both UPSERTS on
	// process_key (pinned by store TestPersistRunsUpsertExecThenExit) rather than
	// doubling. The Windows bridge joins on WSL (distinct boot-id). Only built
	// when linuxebpf.Available passed.
	newEBPFComposite := func() processobs.Backend {
		children := []processobs.Backend{
			linuxebpf.New(linuxebpf.Options{Logger: logger}),
			poll.New(pollOpts),
		}
		if bridge.AvailableInWSL(pc.WindowsBinaryPath) {
			children = append(children, bridge.New(bridgeOpts))
		}
		return processobs.NewComposite(children, onChildErr)
	}

	switch pc.Backend {
	case "off":
		return nil, errors.New("backend set to off")
	case "poll":
		return poll.New(pollOpts), nil
	case "bridge":
		// Cross-OS bridge (§5.5): exec the Windows observer.exe over WSL
		// interop. Start fail-opens if not under WSL or the binary is missing.
		return bridge.New(bridgeOpts), nil
	case "both":
		// Explicit: Linux poll + Windows bridge together (see newComposite).
		return newComposite(), nil
	case "auto":
		// Prefer the real-time eBPF stack when this box can actually load BPF
		// (capability-probed; the unprivileged daemon usually can't, so this is
		// normally false → poll). When eBPF is available, compose it with poll
		// (metrics + initial snapshot, dedup on process_key) + the bridge.
		// Otherwise the legacy path: poll+bridge on WSL, else the single poll
		// backend (Linux /proc or, on a Windows host, the ToolHelp snapshot).
		if linuxebpf.Available(logger) {
			logger.Info("process observability: auto selected linux_ebpf — real-time capture + poll metric sampler")
			return newEBPFComposite(), nil
		}
		if bridge.AvailableInWSL(pc.WindowsBinaryPath) {
			return newComposite(), nil
		}
		return poll.New(pollOpts), nil
	case "linux_ebpf":
		// Explicitly request the real-time Linux stack (§5.2): catches the
		// sub-poll-interval processes the /proc poller misses. The P0 gate is
		// privilege + kernel: loading BPF needs CAP_BPF+CAP_PERFMON and a
		// BPF-capable kernel, which the unprivileged daemon usually lacks — so
		// probe and FAIL OPEN to the poll path rather than disabling capture.
		if !linuxebpf.Available(logger) {
			logger.Info("process observability: linux_ebpf unavailable (needs CAP_BPF+CAP_PERFMON and a BPF-capable kernel) — falling back to poll capture")
			if bridge.AvailableInWSL(pc.WindowsBinaryPath) {
				return newComposite(), nil
			}
			return poll.New(pollOpts), nil
		}
		logger.Info("process observability: linux_ebpf backend active — real-time fork/exec/exit capture + poll metric sampler")
		return newEBPFComposite(), nil
	case "etw":
		// ETW is the high-fidelity Windows backend (§5.2: real-time fork/exec/
		// exit + network/file providers); the poll backend (ToolHelp snapshot)
		// already captures Windows process trees today.
		return nil, errors.New(`etw backend not yet implemented (§5.2 high-fidelity follow-on); use backend = "poll" for Windows process capture`)
	case "endpointsecurity":
		return nil, errors.New("endpointsecurity backend not yet implemented (P6)")
	default:
		return nil, fmt.Errorf("unknown process backend %q", pc.Backend)
	}
}

// resolveProcessPollIntervals turns the [observer.process] config into the
// concrete (Linux-poll, Windows-bridge) snapshot cadences in milliseconds.
// PollIntervalMS is the single operator-facing "process poll rate"; a value
// <= 0 falls back to the 2000 ms default. BridgePollIntervalMS is an optional
// per-bridge override: <= 0 inherits the resolved poll interval so one knob
// controls both sources unless the operator deliberately splits them.
func resolveProcessPollIntervals(pc config.ProcessConfig) (pollMS, bridgeMS int) {
	pollMS = pc.PollIntervalMS
	if pollMS <= 0 {
		pollMS = 2000
	}
	bridgeMS = pc.BridgePollIntervalMS
	if bridgeMS <= 0 {
		bridgeMS = pollMS
	}
	return pollMS, bridgeMS
}

// buildProcessScrubber maps the [observer.process] config into the pure
// FieldScrubber, injecting the existing secret scrubber as the redactor so
// argv/env/path previews never carry credentials (spec §12.2).
func buildProcessScrubber(pc config.ProcessConfig) *processobs.FieldScrubber {
	return &processobs.FieldScrubber{
		ArgvMode:        pc.Argv.Mode,
		MaxPreviewBytes: pc.Argv.MaxPreviewBytes,
		StoreArgCount:   pc.Argv.StoreArgCount,
		EnvEnabled:      pc.Env.Enabled,
		EnvAllowlist:    pc.Env.Allowlist,
		StorePathHash:   pc.Env.StorePathHash,
		Redact: func(s string) string {
			masked, _ := scrub.MaskSecrets(s, func(scrub.TypedFinding) bool { return true })
			return masked
		},
	}
}
