//go:build !linux

package poll

import "github.com/marmutapp/superbased-observer/internal/processobs"

// NewDeepEnricher has no /proc to read on non-Linux hosts, so it returns nil
// — the Observer treats a nil DeepEnricher as "no deep enrichment". (The poll
// backend itself returns ErrUnsupported on these OSes, so this path never
// runs in practice; the function exists so the cross-platform cmd wiring
// compiles.) Native Windows/macOS deep enrichment lands with their backends
// (P6).
func NewDeepEnricher(_ *processobs.FieldScrubber, _ bool, _ int) processobs.DeepEnricher {
	return nil
}
