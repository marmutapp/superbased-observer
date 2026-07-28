//go:build !linux

package linuxebpf

import (
	"context"
	"log/slog"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// backend is the non-Linux stub: eBPF capture is Linux-only, so New returns a
// Backend whose Start fails open with ErrUnsupported. This file exists purely so
// the package — and anything that imports it (the backend selector) — compiles
// and cross-compiles for darwin/windows unchanged (mirrors poll's
// enum_other.go).
type backend struct{}

// New builds the non-Linux stub Backend. It records the network-accounting
// status as UNAVAILABLE (rather than leaving it unset) so a non-Linux host
// reports "not measured" honestly instead of implying a measured zero: eBPF
// cannot see Windows or macOS processes, and per-process byte accounting there
// needs ETW / EndpointSecurity, which are not implemented.
func New(opts Options) processobs.Backend {
	if opts.NetworkAccounting {
		opts.NetworkStatus.Set(processobs.NetworkAccountingUnavailable,
			"per-process network bytes need eBPF (Linux); Windows needs ETW, macOS EndpointSecurity — neither implemented")
	}
	return &backend{}
}

func (b *backend) Name() string { return "linux_ebpf" }

func (b *backend) Start(context.Context) (<-chan processobs.RawEvent, error) {
	return nil, ErrUnsupported
}

func (b *backend) Close() error { return nil }

// Available is always false off Linux, so the selector never chooses the eBPF
// backend on darwin/windows.
func Available(*slog.Logger) bool { return false }
