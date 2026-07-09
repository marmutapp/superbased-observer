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

// New builds the non-Linux stub Backend.
func New(opts Options) processobs.Backend { return &backend{} }

func (b *backend) Name() string { return "linux_ebpf" }

func (b *backend) Start(context.Context) (<-chan processobs.RawEvent, error) {
	return nil, ErrUnsupported
}

func (b *backend) Close() error { return nil }

// Available is always false off Linux, so the selector never chooses the eBPF
// backend on darwin/windows.
func Available(*slog.Logger) bool { return false }
