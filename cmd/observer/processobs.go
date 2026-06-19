package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
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

	obs := processobs.NewObserver(processobs.Options{
		Backend:             backend,
		Attributor:          attributor,
		DeepEnricher:        deepEnricher,
		Sink:                st,
		CaptureUnattributed: captureUnattributed,
		BatchSize:           cfg.Observer.Process.BatchSize,
		MaxTracked:          processObserverMaxTracked,
	})

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
// fail-opens). Only the poll backend is implemented today; "auto" resolves to
// it until the high-fidelity linux_ebpf / etw / endpointsecurity backends
// land. Branching is on the configured capability, not host identity (the
// backend's own Start probe decides OS support and fail-opens).
func selectProcessBackend(pc config.ProcessConfig, logger *slog.Logger) (processobs.Backend, error) {
	pollMS, bridgeMS := resolveProcessPollIntervals(pc)
	pollOpts := poll.Options{Interval: time.Duration(pollMS) * time.Millisecond}
	bridgeOpts := bridge.Options{
		WindowsBinaryPath: pc.WindowsBinaryPath,
		PollIntervalMS:    bridgeMS,
		Logger:            logger,
	}
	// newComposite runs the Linux /proc poll backend AND the Windows cross-OS
	// bridge together (fan-in), so a daemon on the canonical WSL topology
	// captures BOTH WSL-native AND Windows AI-tool processes — a single backend
	// sees only one OS's process table. Fail-open per child inside the composite.
	newComposite := func() processobs.Backend {
		return processobs.NewComposite([]processobs.Backend{
			poll.New(pollOpts),
			bridge.New(bridgeOpts),
		}, func(name string, err error) {
			logger.Warn("process observability: composite child unavailable (capture continues with the rest)", "child", name, "err", err)
		})
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
		// On the canonical WSL topology the daemon can't see Windows-host
		// processes from /proc, so when this is WSL AND a Windows observer.exe
		// resolves, run BOTH the Linux poll backend and the Windows bridge
		// (capture every process source available). Otherwise the single poll
		// backend (Linux /proc or, on a Windows host, the ToolHelp snapshot).
		if bridge.AvailableInWSL(pc.WindowsBinaryPath) {
			return newComposite(), nil
		}
		return poll.New(pollOpts), nil
	case "linux_ebpf":
		return nil, errors.New(`linux_ebpf backend not yet implemented (gated on the P0 WSL check; use backend = "poll")`)
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
