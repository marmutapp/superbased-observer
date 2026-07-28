package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
	"github.com/marmutapp/superbased-observer/internal/processobs/poll"
)

// newProcessBridgeCmd builds `observer process-bridge` — the capturer half of
// the cross-OS bridge (spec §5.5). The WSL daemon execs this Windows-native
// binary over WSL interop and reads its stdout: it runs the poll backend
// (ToolHelp + PEB enrichment on Windows) and streams normalized process events
// as NDJSON Frames, holding no DB and no attribution state. All
// scrub/attribute/store runs in the WSL daemon. Hidden — it is plumbing
// invoked by the bridge backend, not an operator command.
//
// It is OS-agnostic by construction (the poll backend enumerates /proc on
// Linux and ToolHelp on Windows); the bridge's intended deployment is the
// Windows binary, but keeping the command cross-OS lets its stream plumbing be
// tested on the dev host.
func newProcessBridgeCmd() *cobra.Command {
	var (
		intervalMS  int
		useETW      bool
		connectAddr string
		tokenFile   string
	)
	cmd := &cobra.Command{
		Use:    "process-bridge",
		Short:  "Stream local process events as NDJSON for the WSL cross-OS bridge (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := processBridgeOptions{
				Interval: time.Duration(intervalMS) * time.Millisecond,
				ETW:      useETW,
			}
			// Two transports, one stream. --connect dials the daemon's
			// loopback listener (the elevated-Windows-capturer path, plan
			// §0.2); the default writes to stdout for the daemon-spawns-us
			// path. Only the io.Writer differs.
			if connectAddr != "" {
				return runProcessBridgeConnect(cmd.Context(), connectAddr, tokenFile, opts, cmd.ErrOrStderr())
			}
			return streamProcessBridgeWith(cmd.Context(), os.Stdout, opts)
		},
	}
	cmd.Flags().IntVar(&intervalMS, "interval-ms", 2000, "poll interval in milliseconds")
	cmd.Flags().BoolVar(&useETW, "etw", false,
		"additionally start an ETW network capture for per-process byte counters (Windows only; requires an ELEVATED process — fails open to poll-only capture)")
	cmd.Flags().StringVar(&connectAddr, "connect", "",
		"dial this host:port (the observer daemon's loopback listener) and stream frames over the socket instead of stdout; reconnects forever")
	cmd.Flags().StringVar(&tokenFile, "token-file", "",
		"file holding the daemon's shared token for --connect (or set "+processBridgeTokenEnv+"); there is deliberately no --token flag because argv is world-readable")
	return cmd
}

// processBridgeOptions configures one capturer run. It exists so `--etw` can
// be threaded in without touching streamProcessBridge's pinned signature
// (additive, CLAUDE.md rule 6).
type processBridgeOptions struct {
	// Interval is the poll cadence.
	Interval time.Duration
	// ETW requests the additive ETW network capture. Failure to start it is
	// NOT fatal — see streamProcessBridgeWith.
	ETW bool
	// startETW overrides the platform ETW starter. Tests inject a fake; nil
	// uses startETWNetworkCapture (real on Windows, a typed refusal elsewhere).
	startETW func() (etwNetworkCapture, error)
}

// streamProcessBridge runs the poll backend and writes every RawEvent it
// produces to w as an NDJSON Frame, prefixed by a hello frame. It returns when
// ctx is cancelled, the backend closes its channel, or a write fails (stdout
// closed → the WSL reader is gone → exit cleanly). Extracted from the command
// so the stream contract is unit-testable. A backend Start error (unsupported
// OS) is returned so the bridge backend surfaces it as degraded health.
//
// This is the poll-only capture the bridge has always run; its signature is
// pinned by process_bridge_test.go. ETW-augmented runs go through
// streamProcessBridgeWith.
func streamProcessBridge(ctx context.Context, w io.Writer, interval time.Duration) error {
	return streamProcessBridgeWith(ctx, w, processBridgeOptions{Interval: interval})
}

// streamProcessBridgeWith is streamProcessBridge with the full option set.
//
// When opts.ETW is set it starts the ETW network capture and feeds its
// per-pid CUMULATIVE counters into poll.Options.NetworkBytes — the
// processobs.NetworkSampler capability seam, the same one the Linux daemon
// uses to hand eBPF's counters to the poll metric sampler. Nothing downstream
// branches on which OS produced them; the poll backend stamps
// HasNetworkMetrics on the events it already emits.
//
// FAIL-OPEN is the point, not a nicety: ETW session control ALWAYS requires
// elevation, so a non-elevated run is the COMMON case and returns
// ERROR_ACCESS_DENIED. That must cost nothing — process trees and CPU/mem/disk
// keep streaming exactly as before, the accounting mode is reported as
// "unavailable" with the real reason, and no sampler is installed, so events
// carry HasNetworkMetrics=false (UNMEASURED) rather than fabricated zeroes.
func streamProcessBridgeWith(ctx context.Context, w io.Writer, opts processBridgeOptions) error {
	pollOpts := poll.Options{Interval: opts.Interval}

	// Default posture: nothing is measuring bytes because nothing was asked
	// to. "off" is the honest mode for that — distinct from "unavailable",
	// which means it was asked for and could not be delivered. The reason
	// names the MECHANISM rather than implying the operator declined: the
	// capturer only knows it was launched without --etw.
	netMode, netReason := processobs.NetworkAccountingOff,
		"the process capturer was started without --etw, so no per-process network byte counters are collected"
	var etwErr error
	etwLive := false
	// decodeStats is the capture's decode-health reader, or nil when there is
	// no decoder to report on. nil IS the absence — see reportCapturerStats.
	var decodeStats func() (processobs.CapturerDecodeStats, bool)

	if opts.ETW {
		start := opts.startETW
		if start == nil {
			start = startETWNetworkCapture
		}
		capture, err := start()
		if err != nil {
			etwErr = err
			netMode = processobs.NetworkAccountingUnavailable
			netReason = fmt.Sprintf("ETW network capture could not start: %v", err)
		} else {
			defer capture.Close() //nolint:errcheck // best-effort stop
			// Capability seam: the capture is a processobs.NetworkSampler, and
			// that is ALL the poll backend is told about it.
			pollOpts.NetworkBytes = capture.NetworkBytes
			netMode, netReason = capture.Status()
			etwLive = bridge.IsMeasuringNetworkMode(netMode)
			decodeStats = capture.DecodeStats
		}
	}

	backend := poll.New(pollOpts)
	ch, err := backend.Start(ctx)
	if err != nil {
		return fmt.Errorf("process-bridge: backend start: %w", err)
	}
	defer backend.Close() //nolint:errcheck // best-effort stop

	// Name what actually ran. A run that asked for ETW and did not get it is
	// still producing poll events and nothing else, so it reports "poll" —
	// claiming "etw" there would make the far side trust a feed that does not
	// exist. Only a capture that is really counting earns the "+etw" suffix.
	backendName := backend.Name()
	if etwLive {
		backendName += "+etw"
	}

	enc := bridge.NewEncoder(w)
	if err := enc.Hello(bridge.Hello{
		Backend:                 backendName,
		BootID:                  backend.BootID(),
		OS:                      runtime.GOOS,
		PID:                     os.Getpid(),
		NetworkAccountingMode:   netMode,
		NetworkAccountingReason: netReason,
	}); err != nil {
		return nil // stdout already gone; nothing to stream to
	}
	if etwErr != nil {
		// In-band, so the daemon logs it as a capturer diagnostic. Deliberately
		// NOT stderr: the bridge splices a crashed capturer's stderr tail into
		// its respawn error, and an expected non-elevated refusal is not a
		// crash.
		if err := enc.Errorf("etw: network capture unavailable, continuing with poll-only capture: %v", etwErr); err != nil {
			return nil
		}
	}

	// Report the decoder's health IMMEDIATELY and then on a heartbeat. The
	// first report is not redundant even though both counters are zero: it is
	// what tells the daemon a decoder EXISTS on this link, which is a
	// different fact from the silence a non-elevated capturer produces, and
	// the whole surface turns on that distinction.
	reportCapturerStats(enc, decodeStats)
	statsTicker := newCapturerStatsTicker(decodeStats)
	defer statsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statsTicker.C:
			// A failed stats write is NOT a reason to stop capturing: it is a
			// diagnostic, and events are the product. The next event write
			// will discover a dead consumer anyway.
			reportCapturerStats(enc, decodeStats)
		case ev, ok := <-ch:
			if !ok {
				return nil // backend closed the channel (ctx cancel / Close)
			}
			if err := enc.Event(ev); err != nil {
				return nil // write failed → the WSL consumer closed the pipe
			}
		}
	}
}

// capturerStatsInterval is the decode-health heartbeat. It matches
// diag.ProcessHealthRefreshInterval in spirit: often enough that a drop shows
// up on the operator's next dashboard poll, rare enough to be free on a link
// whose real product is process events.
const capturerStatsInterval = 30 * time.Second

// newCapturerStatsTicker returns a ticker that fires the decode-health
// heartbeat, or one that never fires when there is no decoder to report on.
//
// A stopped ticker's channel is nil-safe to select on (it simply never
// delivers), so the caller needs no second branch for the absent case.
func newCapturerStatsTicker(read func() (processobs.CapturerDecodeStats, bool)) *time.Ticker {
	t := time.NewTicker(capturerStatsInterval)
	if read == nil {
		t.Stop()
	}
	return t
}

// reportCapturerStats emits one decode-health frame, and emits NOTHING when
// there is no decoder to report on.
//
// The silence is the honest answer, not a missing feature: a capturer with no
// running network decoder — every non-elevated run, and every non-Windows one
// — has decoded no events at all, and "0 dropped" would tell the operator the
// payload-length assumptions were tested and held. The daemon renders the
// absence as absence.
//
// Write failures are swallowed: this is a diagnostic on a stream whose product
// is events, and the event path already detects a departed consumer.
func reportCapturerStats(enc *bridge.Encoder, read func() (processobs.CapturerDecodeStats, bool)) {
	if read == nil {
		return
	}
	s, ok := read()
	if !ok {
		return
	}
	_ = enc.Stats(bridge.CapturerStats{
		NetworkDecodeDropped:            s.NetworkDropped,
		NetworkDecodeUnsupportedVersion: s.NetworkUnsupportedVersion,
		// The positive half. Without it the daemon can only tell whether the
		// decoder REFUSED anything, and a provider whose event ids were
		// renumbered refuses nothing while decoding nothing.
		NetworkDecodeDecoded: s.NetworkDecoded,
		NetworkDecodeIgnored: s.NetworkIgnored,
	})
}
