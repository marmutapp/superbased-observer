package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
	"github.com/marmutapp/superbased-observer/internal/processobs/poll"
	"github.com/spf13/cobra"
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
	var intervalMS int
	cmd := &cobra.Command{
		Use:    "process-bridge",
		Short:  "Stream local process events as NDJSON for the WSL cross-OS bridge (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamProcessBridge(cmd.Context(), os.Stdout, time.Duration(intervalMS)*time.Millisecond)
		},
	}
	cmd.Flags().IntVar(&intervalMS, "interval-ms", 2000, "poll interval in milliseconds")
	return cmd
}

// streamProcessBridge runs the poll backend and writes every RawEvent it
// produces to w as an NDJSON Frame, prefixed by a hello frame. It returns when
// ctx is cancelled, the backend closes its channel, or a write fails (stdout
// closed → the WSL reader is gone → exit cleanly). Extracted from the command
// so the stream contract is unit-testable. A backend Start error (unsupported
// OS) is returned so the bridge backend surfaces it as degraded health.
func streamProcessBridge(ctx context.Context, w io.Writer, interval time.Duration) error {
	backend := poll.New(poll.Options{Interval: interval})
	ch, err := backend.Start(ctx)
	if err != nil {
		return fmt.Errorf("process-bridge: backend start: %w", err)
	}
	defer backend.Close() //nolint:errcheck // best-effort stop

	enc := bridge.NewEncoder(w)
	if err := enc.Hello(bridge.Hello{
		Backend: backend.Name(),
		BootID:  backend.BootID(),
		OS:      runtime.GOOS,
		PID:     os.Getpid(),
	}); err != nil {
		return nil // stdout already gone; nothing to stream to
	}

	for {
		select {
		case <-ctx.Done():
			return nil
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
