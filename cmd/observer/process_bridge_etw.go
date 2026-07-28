package main

import "github.com/marmutapp/superbased-observer/internal/processobs"

// etwNetworkCapture is the ONLY thing the capturer knows about the ETW stack:
// a live per-process byte counter plus its own honest status. It is the local
// spelling of processobs.NetworkSampler (NetworkBytes) widened by the two
// lifecycle methods the capturer needs, so internal/processobs/etw's concrete
// types never spread past this file (CLAUDE.md rule 2 — one seam per
// integration point, no type leakage past it).
//
// Declaring it here, in an untagged file, is what lets the Linux build of
// cmd/observer compile and be tested without importing the Windows-only ETW
// package at all: the tagged pair below supplies the single constructor.
type etwNetworkCapture interface {
	// NetworkBytes reports CUMULATIVE bytes for one pid — satisfying
	// processobs.NetworkSampler. ok=false means accounting is not live
	// (UNMEASURED); ok=true with (0,0) means measured and idle.
	NetworkBytes(pid int) (in, out int64, ok bool)
	// Status reports the accounting mode/reason in the
	// processobs.NetworkAccounting* vocabulary.
	Status() (mode, reason string)
	// DecodeStats reports the trace session's own decode counters, which are
	// the ONLY evidence that the fixed-offset payload layout this capturer
	// decodes by actually holds on this host.
	//
	// ok=false means there is no decoder to report on — no session was ever
	// created (the common non-elevated run), or this is not Windows. The
	// caller must then send NOTHING, because zeroed counters would claim the
	// layout assumptions were exercised and held when nothing was decoded at
	// all. The bool is load-bearing for exactly the reason
	// processobs.TransportStatsSource's is: Go cannot conditionally implement
	// a method, so presence has to be a value.
	DecodeStats() (processobs.CapturerDecodeStats, bool)
	// Close stops the trace session.
	Close() error
}
