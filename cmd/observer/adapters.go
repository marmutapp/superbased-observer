package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// newAdaptersCmd renders the adapter Integration Capability Registry as a
// support matrix — the agnostic asset that replaces a hand-maintained audit
// grid (adapter-coverage-parity plan §7). Every cell is read from
// internal/integration so the matrix can never drift from the code that
// init/register/doctor actually dispatch on.
func newAdaptersCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "adapters",
		Short: "Show the adapter capability matrix (proxy / hook / MCP / native / token)",
		Long: "Renders the Integration Capability Registry (internal/integration) as\n" +
			"a support matrix: for every adapter, how it can be proxy-routed, how\n" +
			"hooks register, where MCP config is written, which native-console\n" +
			"rails the vendor exposes, and its token/cost capture tier + any known\n" +
			"gap. This is the same data init/register/doctor dispatch on, so the\n" +
			"matrix is generated, never hand-maintained.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			caps := integration.Capabilities()
			sort.Slice(caps, func(i, j int) bool { return caps[i].Tool < caps[j].Tool })
			if jsonOut {
				body, _ := json.MarshalIndent(caps, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			renderAdapterMatrix(cmd.OutOrStdout(), caps)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the registry as JSON instead of a table")
	return cmd
}

// renderAdapterMatrix writes the capability matrix as an aligned table.
func renderAdapterMatrix(w io.Writer, caps []integration.Capability) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ADAPTER\tPROXY\tSURFACE\tHOOK\tMCP\tNATIVE\tTOKEN\tHANDOFF\tATTACH\tRESUME")
	for _, c := range caps {
		fmt.Fprintf(
			tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Tool,
			proxyCell(c.Proxy),
			routabilityCell(c.Routability),
			hookCell(c.Hook),
			mcpCell(c.MCP),
			nativeCell(c.Native),
			tokenCell(c.TokenTier),
			handoffCell(c.Handoff),
			attachCell(c.Attach),
			resumeCell(c),
		)
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "legend\tPROXY = route observer applies today (or dash). SURFACE = the")
	fmt.Fprintln(tw, "\tsurface-specific routability bucket (routable / after-upstream /")
	fmt.Fprintln(tw, "\tafter-bridge / probe / native-exempt). TOKEN shows the capture")
	fmt.Fprintln(tw, "\ttier, with (gap) flagging a known hole. HANDOFF shows the")
	fmt.Fprintln(tw, "\tsession-handoff transcript tier + launch mode (seeded /")
	fmt.Fprintln(tw, "\tdoc-assisted); a dash is the actions-only carry floor (file")
	fmt.Fprintln(tw, "\tlane only, not launchable in the embedded terminal). ATTACH =")
	fmt.Fprintln(tw, "\tcan hand the PTY to the daemon for dashboard jump-in")
	fmt.Fprintln(tw, "\t(`observer <x> --attach`). RESUME = how a closed session")
	fmt.Fprintln(tw, "\treopens: native (the tool's own resume) / handoff (a")
	fmt.Fprintln(tw, "\t--continue-from fork) / dash (neither).")
	_ = tw.Flush()
}

// routabilityCell renders the surface-specific routability bucket — the
// honest "is this routable at all?" status, distinct from the PROXY cell
// (which shows only what observer drives today). A row can read PROXY="—"
// yet SURFACE="routable" (knob exists, writer pending) or SURFACE="probe"
// (BYOK path documented, unconfirmed live).
func routabilityCell(s integration.RouteStatus) string {
	switch s {
	case integration.RouteStatusRoutableNow:
		return "routable"
	case integration.RouteStatusAfterUpstream:
		return "after-upstream"
	case integration.RouteStatusAfterBridge:
		return "after-bridge"
	case integration.RouteStatusProbeRequired:
		return "probe"
	case integration.RouteStatusNativeExempt:
		return "native-exempt"
	default:
		return "—"
	}
}

func proxyCell(p *integration.ProxyRoute) string {
	if p == nil {
		return "—"
	}
	switch p.Kind {
	case integration.RouteEnvSettings:
		return "env:" + p.EnvVar
	case integration.RouteConfigFile:
		return "config-file"
	case integration.RouteLauncher:
		return "launcher"
	default:
		return string(p.Kind)
	}
}

func hookCell(h integration.HookSpec) string {
	if h.Mechanism == integration.HookNone {
		return "—"
	}
	s := string(h.Mechanism)
	if h.CrossOSBridge {
		s += "+bridge"
	}
	if !h.AutoWired {
		s += "+manual"
	}
	return s
}

func mcpCell(m *integration.MCPTarget) string {
	if m == nil {
		return "—"
	}
	if !m.Implemented {
		return string(m.Format) + " (candidate)"
	}
	return string(m.Format)
}

func nativeCell(n integration.NativeRails) string {
	if !n.Any() {
		return "—"
	}
	var r []string
	if n.A {
		r = append(r, "A")
	}
	if n.B {
		r = append(r, "B")
	}
	if n.C {
		r = append(r, "C")
	}
	return strings.Join(r, "/")
}

func tokenCell(t integration.TokenTier) string {
	tier := t.Best
	if tier == "" {
		tier = "unknown"
	}
	if t.Gap != "" {
		return tier + " (gap)"
	}
	return tier
}

// handoffCell renders the session-handoff capability compactly: the
// transcript tier the handoff re-reads (full / partial), joined with the
// launch mode when the tool is startable in the dashboard's embedded web
// terminal (seeded prompt injection vs a doc-assisted TUI open). A
// zero-value HandoffCapability — actions-only transcript, not launchable —
// renders as a dash: the honest floor is a metadata-carry handoff over the
// universal file lane, not a grounded transcript or launcher (matching the
// nativeCell/tokenCell zero-value convention; never a fabricated cell).
func handoffCell(h integration.HandoffCapability) string {
	var parts []string
	switch h.Transcript {
	case integration.TranscriptFull:
		parts = append(parts, "full")
	case integration.TranscriptPartial:
		parts = append(parts, "partial")
	}
	if h.Launchable() {
		switch h.Launch.Mode {
		case integration.LaunchDocAssisted:
			parts = append(parts, "doc-assisted")
		default:
			parts = append(parts, "seeded")
		}
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "+")
}

// attachCell renders the session-attach capability: "yes (--attach)" when
// the tool can hand its PTY to the daemon for dashboard jump-in (a non-nil
// AttachSpec), else a dash (the honest floor — bare launch only, not
// retrofittable). Dispatches on the capability SHAPE, never tool name.
func attachCell(a *integration.AttachSpec) string {
	if a == nil {
		return "—"
	}
	return "yes (--attach)"
}

// resumeCell renders how a closed session reopens, dispatching on capability
// shape: "native" when a grounded ResumeNative contract exists; otherwise
// "handoff" when the tool is launchable (the shipped `--continue-from` fork
// runs in a dashboard terminal via the launcher); otherwise a dash (neither
// native resume nor a launcher to fork into). Phase 0 grounds no
// ResumeNative row, so today this reads "handoff" for launchable tools and a
// dash for file-lane-only ones — honest, never fabricated.
func resumeCell(c integration.Capability) string {
	switch {
	case c.Resume.Kind == integration.ResumeNative:
		return "native"
	case c.Handoff.Launchable():
		return "handoff"
	default:
		return "—"
	}
}
