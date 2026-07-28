package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/processbridge/setup"
)

// printProcessBridgeTaskPlan writes the operator-facing message.
//
// It NEVER prints "registered" — nothing here registers anything. The
// Manual/Unknown branches say plainly that observer cannot create the task and
// why, and the Blocked branch names the exact missing dependency instead of
// emitting a command with a placeholder in it.
func printProcessBridgeTaskPlan(out io.Writer, plan setup.Plan) {
	switch plan.Outcome {
	case setup.OutcomeSkip:
		// Silence is the honest output for a feed nobody asked for — with one
		// exception the planner marks with a Reason: a config that reads as
		// enabled ([observer.process.etw] on) while the master switch is off,
		// which captures nothing and would otherwise look fine.
		if plan.Reason != "" {
			fmt.Fprintf(out, "\nprocess capture (ETW): %s\n", plan.Reason)
		}
		return
	case setup.OutcomePresent:
		fmt.Fprintf(out, "\nprocess capture (ETW): Scheduled Task %q is already registered — left untouched.\n", setup.TaskName)
		return
	case setup.OutcomeBlocked:
		fmt.Fprintf(out, "\nprocess capture (ETW): the elevated Scheduled Task cannot be set up yet — %s.\n", plan.Reason)
		fmt.Fprintln(out, "  nothing was registered; process capture continues unchanged without the ETW feed.")
		return
	case setup.OutcomeManual, setup.OutcomeUnknown:
	}

	fmt.Fprintf(out, "\nprocess capture (ETW): Scheduled Task %q is not registered.\n", setup.TaskName)
	if plan.Outcome == setup.OutcomeUnknown {
		fmt.Fprintf(out, "  (in fact schtasks could not tell us either way: %s — treat the command below as\n"+
			"  \"run it if the task is absent\"; without /F schtasks refuses to overwrite an existing task.)\n", plan.Reason)
	}
	fmt.Fprintln(out, "  observer cannot create it: ETW session control always requires elevation, and a WSL")
	fmt.Fprintln(out, "  process cannot self-grant it — `schtasks /Create /SC ONLOGON` is refused with")
	fmt.Fprintln(out, "  \"Access is denied\" from here (measured). Run this ONCE yourself, in an ELEVATED")
	if plan.CmdShellOnly {
		fmt.Fprintln(out, "  Windows COMMAND PROMPT — not PowerShell: your token path contains a space, which")
		fmt.Fprintln(out, "  forces the \\\" escaping that PowerShell rejects (right-click → Run as administrator):")
	} else {
		fmt.Fprintln(out, "  Windows shell — either Command Prompt or PowerShell (right-click → Run as")
		fmt.Fprintln(out, "  administrator). The single quotes are load-bearing: schtasks normalizes them to")
		fmt.Fprintln(out, "  the correct quoted action, and the \\\" form PowerShell rejects. Paste it as-is:")
	}
	fmt.Fprintf(out, "\n    %s\n\n", plan.Command)
	for _, note := range plan.Notes {
		fmt.Fprintf(out, "  - %s\n", note)
	}
	fmt.Fprintf(out, "  verify: schtasks.exe /Query /TN \"%s\" /V /FO LIST\n", setup.TaskName)
	fmt.Fprintf(out, "  remove: schtasks.exe /Delete /TN \"%s\" /F   (also elevated)\n", setup.TaskName)
	fmt.Fprintln(out, "  until then everything else keeps working: the ETW feed is additive, and its absence")
	fmt.Fprintln(out, "  shows up as network_accounting_mode=off rather than as fabricated byte counters.")
}

// initProcessBridgeTaskStep is the `observer init` entry point for the
// elevated-ETW-capturer setup surface (plan §W4). One call site, one plain
// result — the step reads config, probes, and prints; it writes nothing, so it
// is safe on every init path including --dry-run.
//
// Every failure is silent-and-harmless by design: a config that will not load
// means we have no listen address or token path to put in a command, and an
// init must not fail over an advisory hint.
func initProcessBridgeTaskStep(ctx context.Context, out io.Writer, configPath string) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return
	}
	in := setup.ResolveInputs(ctx, cfg.Observer.Process, filepath.Dir(cfg.Observer.DBPath),
		setup.ProductionEnv())
	printProcessBridgeTaskPlan(out, setup.PlanTask(in))
}
