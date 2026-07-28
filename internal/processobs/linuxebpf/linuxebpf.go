// Package linuxebpf is the high-fidelity, real-time Linux process-capture
// backend for Process Observability (docs/process-observability.md §5.2). It
// attaches eBPF programs to the sched_process_exec / sched_process_exit
// tracepoints and streams every fork/exec/exit to userspace through a
// ring buffer, so a command that lives and dies between two poll ticks — the
// blind spot the poll backend (§5.4) cannot see — is still captured.
//
// It implements processobs.Backend and emits the IDENTICAL RawEvent shape the
// poll backend produces (start-time read from the same /proc field → identical
// ProcessKey), so the Attributor, the cross-OS pass, and the action-derived-row
// dedup all consume it unchanged, and a process seen by BOTH backends upserts
// on process_key rather than doubling.
//
// Privilege/kernel reality (the P0 gate): loading BPF and attaching tracepoints
// needs CAP_BPF+CAP_PERFMON (or root); the daemon usually runs unprivileged. So
// the backend capability-probes and FAILS OPEN — Start returns an error the
// daemon turns into degraded health while it keeps running, and the backend
// selector falls back to the poll backend (selectProcessBackend). No CGO: the
// loader is pure-Go (github.com/cilium/ebpf) and the program is built in-process
// from hand-written eBPF asm (no clang, no CO-RE struct walking, no tracefs).
//
// The whole package is Linux-only; backend_other.go provides a stub New on
// every other OS so cross-compilation is unaffected (mirrors the poll backend's
// enum_linux.go / enum_other.go split).
package linuxebpf

import (
	"errors"
	"log/slog"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// ErrUnsupported is returned by Start on a non-Linux build, or when the kernel
// or privileges cannot support eBPF capture. The Observer reports degraded
// health and the daemon continues (fail-open, spec §15); the selector falls
// back to the poll backend.
var ErrUnsupported = errors.New("processobs/linuxebpf: eBPF capture unavailable (non-Linux, or missing CAP_BPF/kernel support)")

// Options configures the eBPF Backend. The zero value is valid — every field
// falls back to a platform default inside New.
type Options struct {
	// BootID stamps every event's ProcessKey input (§9.3). Default: the kernel
	// boot id (same source as the poll backend, so their keys share a namespace).
	BootID string
	// Now overrides time.Now for tests.
	Now func() time.Time
	// Logger receives non-fatal backend diagnostics. Default: slog.Default().
	Logger *slog.Logger
	// NetworkAccounting requests per-process TCP byte counters (the fentry/
	// fexit probes in netprogram_linux.go). Default false — it is an extra
	// privileged attach, so it follows the same opt-in posture as the rest of
	// [observer.process]. When the probes cannot attach the backend degrades
	// to lifecycle-only capture exactly as before; Start never fails because
	// of it.
	NetworkAccounting bool
	// NetworkStatus is the shared handle the backend WRITES the accounting
	// status to (off / unavailable+reason / tcp), so health, doctor and the
	// dashboard can tell "measured zero bytes" from "not measured". Optional:
	// nil means nobody is listening.
	NetworkStatus *processobs.NetworkAccounting
	// enrich resolves a pid to its /proc snapshot; nil = the real /proc reader
	// (Linux). Unexported: tests drive the pure translator directly rather than
	// the Backend, so they never need to inject here.
	enrich enrichFunc
}

func (o *Options) withDefaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}
