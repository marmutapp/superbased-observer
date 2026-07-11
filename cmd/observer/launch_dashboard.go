package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/termsession"
)

// launch_dashboard.go wires the dashboard's embedded web-terminal launch
// seam (dashboard.LaunchManager) to internal/termsession. It is the single
// boundary that translates the dashboard's server-derived LaunchSpec into a
// termsession.Spec — injecting the observer binary path (os.Executable) so a
// client can never influence which binary runs — and maps termsession's
// errors onto the dashboard's status-bearing sentinels. The dashboard never
// imports termsession; this adapter is the only thing that does (the same
// pattern as handoffRunner behind BuildHandoff).

// launchManagerAdapter bridges *termsession.Manager to dashboard.LaunchManager.
type launchManagerAdapter struct {
	mgr     *termsession.Manager
	binPath string
}

// newLaunchManager builds the embedded-terminal launch manager, or returns a
// nil seam (+ no-op close) when [handoff].allow_dashboard_launch is false —
// the dashboard treats a nil LaunchManager as the disabled state (503 + the
// button hidden). The returned close func stops the reaper and kills every
// live session; wire it into the command's teardown.
func newLaunchManager(cfg config.Config, logger *slog.Logger) (dashboard.LaunchManager, func(), error) {
	if !cfg.Handoff.AllowDashboardLaunch {
		return nil, func() {}, nil
	}
	// Leave the launch seam unwired on an OS with no in-process PTY backend
	// (a native-Windows daemon). A nil seam is the dashboard's honest
	// "disabled" state: the "Launch here" button is hidden rather than shown
	// and failing on click. Cross-tool migration is unaffected — the
	// platform-independent "Write handover doc" path runs through
	// BuildHandoff, not this manager. A future ConPTY backend flips
	// termsession.PTYSupported() to true and re-enables the seam here.
	if !termsession.PTYSupported() {
		logger.Info("dashboard launch: embedded terminal disabled — no PTY backend on this OS (run the daemon under WSL/Linux); handoff-doc migration is unaffected")
		return nil, func() {}, nil
	}
	binPath, err := os.Executable()
	if err != nil {
		return nil, func() {}, fmt.Errorf("resolve observer binary for dashboard launch: %w", err)
	}
	mgr := termsession.NewManager(termsession.Options{Logger: logger})
	return &launchManagerAdapter{mgr: mgr, binPath: binPath}, mgr.Shutdown, nil
}

func (a *launchManagerAdapter) Create(spec dashboard.LaunchSpec) (string, error) {
	handle, err := a.mgr.Create(termsession.Spec{
		BinPath:     a.binPath,
		Subcommand:  spec.Subcommand,
		SessionID:   spec.SessionID,
		Carry:       spec.Carry,
		FromMessage: spec.FromMessage,
		Rows:        spec.Rows,
		Cols:        spec.Cols,
	})
	if err != nil {
		return "", mapLaunchErr(err)
	}
	return handle, nil
}

func (a *launchManagerAdapter) Attach(handle string) (dashboard.LaunchSession, error) {
	s, err := a.mgr.Attach(handle)
	if err != nil {
		if errors.Is(err, termsession.ErrAlreadyAttached) {
			return nil, dashboard.ErrLaunchAlreadyAttached
		}
		return nil, err
	}
	return s, nil // *termsession.Session satisfies dashboard.LaunchSession
}

func (a *launchManagerAdapter) Detach(handle string) { a.mgr.Detach(handle) }

func (a *launchManagerAdapter) Resize(handle string, rows, cols uint16) error {
	return a.mgr.Resize(handle, rows, cols)
}

func (a *launchManagerAdapter) Close(handle string) { a.mgr.Close(handle) }

func (a *launchManagerAdapter) Snapshot() []dashboard.LaunchInfo {
	live := a.mgr.Snapshot()
	out := make([]dashboard.LaunchInfo, 0, len(live))
	for _, s := range live {
		out = append(out, dashboard.LaunchInfo{
			ID:         s.ID,
			Subcommand: s.Subcommand,
			SessionID:  s.SessionID,
			CreatedAt:  s.CreatedAt,
			Attached:   s.Attached,
			Exited:     s.Exited,
			ExitCode:   s.ExitCode,
		})
	}
	return out
}

// mapLaunchErr translates termsession's sentinels onto the dashboard's so the
// HTTP handler can pick an honest status without importing termsession.
func mapLaunchErr(err error) error {
	switch {
	case errors.Is(err, termsession.ErrTooManySessions):
		return dashboard.ErrLaunchTooMany
	case errors.Is(err, termsession.ErrPlatformUnsupported):
		return dashboard.ErrLaunchUnsupported
	default:
		return err
	}
}
