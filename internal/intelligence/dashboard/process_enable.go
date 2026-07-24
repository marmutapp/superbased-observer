package dashboard

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
	"github.com/marmutapp/superbased-observer/internal/processobs/linuxebpf"
)

// processBackendRunnable reports whether a [observer.process].backend value
// actually CONSTRUCTS a capture backend in the daemon's selector
// (cmd/observer/processobs.go::selectProcessBackend). It is the dashboard-side
// mirror of that selector's vocabulary — "does the selector build anything for
// this value?" — kept honest against the selector's real cases rather than the
// config-validation vocabulary (which accepts etw/endpointsecurity as spellings
// the selector then refuses).
//
// It is the FIRST of two gates the enable verb applies; the second is
// processCapability.backendRunsHere, which asks the platform-truth question
// "and will the thing the selector builds actually capture on THIS host?".
// Selector-constructs is necessary but not sufficient: on darwin the selector
// builds a poll backend for "poll"/"auto", but poll's platformEnumerate is an
// unsupported stub there (see processCapability), so it captures nothing.
//
// Grounded in the selector's cases (cmd/observer/processobs.go, verified
// 2026-07-24 — it is the ONLY selectProcessBackend in the tree, no build-tagged
// sibling):
//   - "poll" / "bridge" / "both" / "auto" / "linux_ebpf" → a Backend IS
//     constructed. Runnable-as-vocabulary.
//   - "off"              → returns an error ("backend set to off"). Not runnable.
//   - "etw"              → returns an error UNCONDITIONALLY on every platform
//     ("etw backend not yet implemented"). Not runnable.
//   - "endpointsecurity" → returns an error UNCONDITIONALLY on every platform
//     ("endpointsecurity backend not yet implemented (P6)"). Not runnable.
//   - "" / any unknown   → the selector's default returns "unknown process
//     backend %q". Not runnable.
func processBackendRunnable(backend string) bool {
	switch backend {
	case "poll", "bridge", "both", "auto", "linux_ebpf":
		return true
	default:
		// off, "", etw, endpointsecurity, and any unknown value construct
		// nothing in the selector.
		return false
	}
}

// processCapability describes what OS process-capture backends can ACTUALLY
// capture on a given host right now. It is the platform truth the enable verb
// grounds its decisions in — as opposed to processBackendRunnable, which only
// mirrors the selector's construction vocabulary and would (dishonestly) call
// "poll"/"auto" runnable on darwin, where the poll enumerator is a stub.
//
// Each field is grounded in a REAL availability probe in the tree:
//   - PollOK   — the poll backend has a native enumerator only on linux
//     (internal/processobs/poll/enum_linux.go, //go:build linux) and windows
//     (enum_windows.go, //go:build windows); every other GOOS gets the
//     ErrUnsupported stub (enum_other.go, //go:build !linux && !windows). So
//     PollOK ≡ GOOS ∈ {linux, windows}.
//   - EBPFOK   — internal/processobs/linuxebpf.Available: a definitive load+
//     attach probe on linux (probe_linux.go), hard-false off linux
//     (backend_other.go, //go:build !linux).
//   - BridgeOK — internal/processobs/bridge.AvailableInWSL: true only inside WSL
//     interop AND when a Windows observer.exe resolves (resolve.go).
type processCapability struct {
	GOOS     string
	PollOK   bool
	EBPFOK   bool
	BridgeOK bool
}

// anyRunnable reports whether ANY backend can capture on this host. When false,
// the host has no runnable process-capture backend at all (e.g. darwin: poll
// stub, no eBPF off-linux, bridge needs WSL) and the enable verb must refuse
// honestly rather than flip config it can never satisfy.
func (c processCapability) anyRunnable() bool {
	return c.PollOK || c.EBPFOK || c.BridgeOK
}

// backendRunsHere reports whether the given configured backend value will
// actually CAPTURE on this host — both that the selector constructs something
// for it (processBackendRunnable) AND that the constructed backend has a
// working capture path on this platform. This is what distinguishes an explicit
// "bridge" on a non-WSL host (constructs, but captures nothing here → false, so
// the verb switches it to "auto") from "bridge" on the WSL topology (true,
// preserved).
func (c processCapability) backendRunsHere(backend string) bool {
	if !processBackendRunnable(backend) {
		// off / "" / etw / endpointsecurity / unknown — selector builds nothing.
		return false
	}
	switch backend {
	case "poll":
		return c.PollOK
	case "bridge":
		return c.BridgeOK
	case "both":
		// Linux/Windows poll + Windows bridge composite: captures if either leg
		// has a working path here.
		return c.PollOK || c.BridgeOK
	case "linux_ebpf":
		// The selector fails eBPF OPEN to the poll backend when the host can't
		// load BPF, so linux_ebpf captures here whenever eBPF OR poll can.
		return c.EBPFOK || c.PollOK
	case "auto":
		// Automatic host-best selection captures whenever anything can.
		return c.anyRunnable()
	default:
		return false
	}
}

// realProcessCapability returns the production capability probe: it reads the
// live platform truth from the same availability probes the daemon's selector
// uses. Bound in New; overridden by a fixed capability in the enable-capture
// tests so they cover every GOOS shape without the real platform. The
// ProcessConfig argument carries the operator's WindowsBinaryPath so the WSL
// bridge probe resolves the same executable the daemon would.
func realProcessCapability(logger *slog.Logger) func(config.ProcessConfig) processCapability {
	return func(pc config.ProcessConfig) processCapability {
		return processCapability{
			GOOS:     runtime.GOOS,
			PollOK:   runtime.GOOS == "linux" || runtime.GOOS == "windows",
			EBPFOK:   linuxebpf.Available(logger),
			BridgeOK: bridge.AvailableInWSL(pc.WindowsBinaryPath),
		}
	}
}

// enableCaptureResponse is the POST /api/process/enable-capture wire shape. It
// reports the final capture state, whether the verb had to switch a
// non-runnable backend to automatic selection (with the prior value so the UI
// can name it), whether a daemon restart is needed to bind the change, and —
// when this host has no runnable backend at all — an honest reason/detail
// instead of a config flip.
type enableCaptureResponse struct {
	Enabled         bool   `json:"enabled"`
	Backend         string `json:"backend,omitempty"`
	SwitchedBackend bool   `json:"switched_backend"`
	// PreviousBackend is the pre-switch backend value. It is ALWAYS emitted when
	// SwitchedBackend is true (no omitempty — an empty prior value is a real,
	// nameable state: the frontend renders "" as "unset"). It stays "" and
	// serializes as such on the preserved / idempotent / unsupported paths,
	// where the frontend ignores it.
	PreviousBackend string `json:"previous_backend"`
	RestartRequired bool   `json:"restart_required"`
	// Reason + Detail are populated ONLY on the unsupported-platform refusal
	// (Enabled=false, no config write): reason is a stable machine token
	// ("unsupported_platform"), detail an operator-facing sentence.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// handleProcessEnableCapture serves POST /api/process/enable-capture — the
// dedicated, server-side, ATOMIC "turn on OS process capture" verb that
// replaces the frontend's old GET-config → decide → PUT-section dance.
//
// The whole load → decide → validate → write critical section runs under the
// SAME configWriteMu (config.WriteLock()) every other in-process config writer
// holds, so a concurrent section/pricing/backup save or the admission-policy
// persister can never lose this write (and vice versa). Decisions, in order:
//
//   - This host has NO runnable capture backend at all (processCapability.
//     anyRunnable() false — e.g. darwin: poll stub, no eBPF off-linux, bridge
//     needs WSL) → do NOT flip config; respond honestly with enabled=false +
//     reason=unsupported_platform so the frontend explains it instead of
//     promising a capture that can never start.
//   - Already enabled AND the configured backend actually captures on THIS host
//     (backendRunsHere) → nothing to do: respond idempotently WITHOUT writing
//     the file (no .bak churn, no restart banner).
//   - Otherwise set Enabled=true; if the configured backend cannot capture here
//     (off / "" / etw / endpointsecurity / unknown, OR an otherwise-valid value
//     like "bridge" on a non-WSL host), switch it to "auto" (automatic host-best
//     selection, which captures whenever anything can) and flag the switch with
//     the prior value so the UI can name it.
//   - Validate the mutated config the SAME way config.Load does at daemon start
//     (never persist a config the next start would reject), then write.
//
// The process section binds at daemon start, so a write reports
// restart_required=true.
func (s *Server) handleProcessEnableCapture(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.ConfigPath == "" {
		http.Error(w, "config path not configured — server has no file to save to", http.StatusConflict)
		return
	}

	// Serialize the whole load→decide→validate→write against every other
	// in-process config-file writer (Fix 1 lock domain). See Server.configWriteMu.
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()

	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, fmt.Errorf("load current config: %w", err))
		return
	}
	prevBackend := cfg.Observer.Process.Backend

	// Platform-honest refusal: if NOTHING can capture on this host, do not flip
	// config we can never satisfy — report unsupported_platform and leave the
	// file untouched (no write, no restart banner). Proven no-write by the
	// handler test's mtime/.bak assertion.
	pcap := s.processCapabilityFn(cfg.Observer.Process)
	if !pcap.anyRunnable() {
		writeJSON(w, enableCaptureResponse{
			Enabled:         false,
			SwitchedBackend: false,
			RestartRequired: false,
			Reason:          "unsupported_platform",
			Detail:          fmt.Sprintf("process capture has no runnable backend on %s yet", pcap.GOOS),
		})
		return
	}

	// Idempotent short-circuit: capture already on AND the configured backend
	// actually captures here → respond without touching the file.
	if cfg.Observer.Process.Enabled && pcap.backendRunsHere(prevBackend) {
		writeJSON(w, enableCaptureResponse{
			Enabled:         true,
			Backend:         prevBackend,
			SwitchedBackend: false,
			RestartRequired: false,
		})
		return
	}

	cfg.Observer.Process.Enabled = true
	switched := false
	if !pcap.backendRunsHere(prevBackend) {
		cfg.Observer.Process.Backend = "auto"
		switched = true
	}

	// Validate-before-write: the SAME semantic check config.Load runs at daemon
	// start, so this verb can never persist a config the next start would reject.
	if err := config.Validate(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := writeConfigToml(s.opts.ConfigPath, cfg); err != nil {
		writeErr(w, err)
		return
	}
	s.notifyConfigSaved()

	resp := enableCaptureResponse{
		Enabled:         true,
		Backend:         cfg.Observer.Process.Backend,
		SwitchedBackend: switched,
		RestartRequired: true, // the process section binds at daemon start
	}
	if switched {
		// Fix 3: always carry the prior value when we switched, even when it was
		// empty ("" → the frontend names it "unset").
		resp.PreviousBackend = prevBackend
	}
	writeJSON(w, resp)
}
