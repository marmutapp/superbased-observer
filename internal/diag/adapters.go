package diag

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	adapterdefaults "github.com/marmutapp/superbased-observer/internal/adapter/defaults"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/toolresolve"
	"github.com/marmutapp/superbased-observer/internal/toolresolve/host"
)

// resolveEnvOnce builds the production toolresolve.Env exactly once per
// process (one login-shell PATH capture + crossmount walk for the whole
// `observer doctor` run, no matter how many adapters get a launch-resolution
// note).
var resolveEnvOnce = sync.OnceValue(func() toolresolve.Env { return host.NewEnv(host.Options{}) })

// launchResolve is the injectable seam over toolresolve.Resolve — tests swap
// it to exercise every verdict without touching the filesystem or a login
// shell.
var launchResolve = func(spec integration.BinaryResolveSpec) toolresolve.Resolution {
	return toolresolve.Resolve(spec, resolveEnvOnce())
}

// checkAdapters is the registry-driven, all-adapter capture-health
// summary (replaces the old two-tool checkAdapterPaths). For every
// REGISTERED adapter it reports whether it's detected on this host (any
// WatchPaths dir exists), idle (enabled but no local data yet), or
// disabled via the enabled_adapters allow-list. Iterating the registry
// means every adapter — including any future one — is covered uniformly
// without touching this function.
func checkAdapters(cfg config.Config) Check {
	enabled := enabledSet(cfg)
	var detected, idle, foreignOnly, disabled []string
	for _, a := range adapterdefaults.Adapters() {
		name := a.Name()
		if enabled != nil && !enabled[name] {
			disabled = append(disabled, name)
			continue
		}
		if adapterDetected(a) {
			detected = append(detected, name)
			continue
		}
		// Not detected locally — before filing it as merely idle, check
		// whether it's actually installed on Windows only (a WSL daemon
		// can't launch it). Resolution only runs for this branch (idle/
		// undetected tools), and shares the one memoized login-PATH env.
		if ic, _ := integration.For(name); ic.Binary != nil {
			if res := launchResolve(*ic.Binary); res.Verdict == toolresolve.VerdictForeignOnly {
				foreignOnly = append(foreignOnly, name)
				continue
			}
		}
		idle = append(idle, name)
	}
	sort.Strings(detected)
	sort.Strings(idle)
	sort.Strings(foreignOnly)
	sort.Strings(disabled)

	var details []string
	if len(detected) > 0 {
		details = append(details, "detected (local data dir present): "+strings.Join(detected, ", "))
	}
	if len(idle) > 0 {
		details = append(details, "enabled, no data yet: "+strings.Join(idle, ", "))
	}
	if len(foreignOnly) > 0 {
		details = append(details, "installed on Windows only (not launchable from WSL): "+strings.Join(foreignOnly, ", "))
	}
	if len(disabled) > 0 {
		details = append(details, "disabled (enabled_adapters): "+strings.Join(disabled, ", "))
	}
	details = append(details, "run `observer doctor <tool>` for a per-adapter capture check")

	msg := strconv.Itoa(len(detected)) + " adapter(s) with local data, " + strconv.Itoa(len(idle)) + " idle"
	if len(detected) == 0 {
		msg = "no adapter data directories detected yet"
	}
	return Check{Name: "adapters", Status: StatusOK, Message: msg, Details: details}
}

// CheckAdapter runs a focused, per-adapter capture-health check (the
// `observer doctor <tool>` surface). It returns ok=false when tool is
// not a registered adapter (so the caller can fall back to a substring
// filter over the other checks). For proxy-routable tools it adds the
// routing pre-flight (proxy reachable + base-URL pointed at the proxy +
// Azure bypass warning); for everything else it confirms the
// watcher/hook capture path. Works for ALL adapters, not just opencode.
func CheckAdapter(tool string, cfg config.Config) (Check, bool) {
	var found adapter.Adapter
	for _, a := range adapterdefaults.Adapters() {
		if a.Name() == tool {
			found = a
			break
		}
	}
	if found == nil {
		return Check{}, false
	}

	var details []string
	worst := StatusOK
	note := func(s Status, m string) {
		details = append(details, m)
		if s > worst {
			worst = s
		}
	}

	if enabled := enabledSet(cfg); enabled != nil && !enabled[tool] {
		note(StatusWarn, "disabled in [observer.watch] enabled_adapters — the watcher will skip it (add \""+tool+"\" to re-enable)")
	}

	var present []string
	for _, p := range found.WatchPaths() {
		if dirExists(p) {
			present = append(present, p)
		}
	}
	if len(present) > 0 {
		note(StatusOK, "watch path present: "+strings.Join(present, ", "))
	} else {
		hint := strings.Join(found.WatchPaths(), ", ")
		if hint == "" {
			hint = "(no canonical path on this OS)"
		}
		note(StatusWarn, "no watch path found ("+hint+") — is "+tool+" installed, and has it created a session yet?")
	}

	ic, _ := integration.For(tool)
	if ic.Proxy != nil {
		ri := ic.Proxy
		port := cfg.Proxy.Port
		if port <= 0 {
			port = 8820
		}
		portStr := strconv.Itoa(port)
		addr := "127.0.0.1:" + portStr
		if dialable(addr) {
			note(StatusOK, "observer proxy reachable at "+addr)
		} else {
			note(StatusWarn, "observer proxy NOT reachable at "+addr+" — start `observer start` for proxy-level api_turn capture")
		}

		switch {
		case ri.Note != "":
			note(StatusOK, ri.Note+" — launch with `"+ri.Launcher+"`")
		case ri.EnvVar != "":
			v := strings.TrimSpace(os.Getenv(ri.EnvVar))
			switch {
			case v == "":
				note(StatusWarn, ri.EnvVar+" not set — launch via `"+ri.Launcher+"` (or export "+ri.EnvVar+"=http://127.0.0.1:"+portStr+ri.Suffix+") for proxy capture")
			case strings.Contains(v, "127.0.0.1:"+portStr) || strings.Contains(v, "localhost:"+portStr):
				note(StatusOK, ri.EnvVar+" points at the observer proxy ("+v+")")
			default:
				note(StatusWarn, ri.EnvVar+"="+v+" is NOT the observer proxy — proxy-level api_turns won't be captured for "+tool)
			}
		}

		// OpenAI-compatible routable tools can be diverted by an
		// Azure-direct provider that ignores the base-URL env var.
		if ri.Suffix == "/v1" && azureProviderConfigured() {
			note(StatusWarn, "Azure provider detected (AZURE_* env) — Azure-direct calls may bypass "+ri.EnvVar+"; route the provider base URL through the observer proxy for api_turn capture")
		}
	} else {
		note(StatusOK, "captured via the watcher/hooks (no proxy launcher needed for this adapter)")
	}

	// Launch-binary resolution (registry-sourced BinaryResolveSpec). Only
	// tools the daemon can actually spawn (`observer <tool>`) carry a spec;
	// a nil Binary means no grounded resolution row for this adapter.
	if ic.Binary != nil {
		status, msg := launchResolutionNote(tool, launchResolve(*ic.Binary))
		note(status, msg)
	}

	// Native-console ledger (Phase 4, registry-sourced). Informational:
	// whether the VENDOR exposes managed-config / usage-export / org-
	// analytics rails. Most adapters are enrollment-only — that is the
	// honest ceiling, not a fault, so it stays StatusOK.
	note(StatusOK, "native console: "+nativeRailsSummary(ic.Native))

	// Token/cost coverage gauge (Phase 5, registry-sourced). Names the best
	// available capture tier and any honest known gap so coverage is
	// measured uniformly across adapters and we can watch the gaps shrink.
	// Stays StatusOK even when gapped: the gaps are inherent to the tool's
	// source data, not user-fixable misconfiguration — surfacing them as a
	// WARN would be alarm fatigue, not signal.
	note(StatusOK, "token/cost: "+tokenTierSummary(ic.TokenTier))

	// Native tool VOCABULARY coverage (WP-T7, registry-sourced): whether
	// the canonical taxonomy (internal/tooltax) carries this adapter's
	// native tool names, so its raw tool calls resolve to a canonical
	// action type instead of falling into `unknown`. An honest-zero row
	// prints its registry Note VERBATIM — the Note is the grounded reason
	// (e.g. a browser-capture lane that records chat turns, never tool
	// calls), and paraphrasing it would re-introduce exactly the
	// heuristic-lie class WP-T3 removed. Stays StatusOK either way: a
	// capture with no tool vocabulary is the honest ceiling of that
	// source, not user-fixable misconfiguration.
	note(StatusOK, "vocabulary: "+vocabularySummary(ic.Vocabulary))

	// Session attach/resume (registry-sourced, session-attach design §2.3).
	// Informational: whether the tool can hand its PTY to the daemon for
	// dashboard jump-in (`observer <sub> --attach`) and how a closed session
	// reopens (native resume vs the shipped handoff-fork). Stays StatusOK —
	// a non-attachable tool is the honest floor, not a fault.
	note(StatusOK, "session: "+attachResumeSummary(ic))

	msg := tool + " capture path looks good"
	if worst != StatusOK {
		msg = tool + " — see notes below"
	}
	return Check{Name: tool, Status: worst, Message: msg, Details: details}, true
}

// launchResolutionNote renders one tool's toolresolve.Resolution as a doctor
// note: the status (OK for a clean resolve, Warn for anything the operator
// should act on) and the one-line honest message. It shares the same verdict
// vocabulary as toolresolve.FormatVerdict but stays doctor-note-shaped (one
// line, no leading "tool:" — CheckAdapter already scopes notes to tool).
func launchResolutionNote(tool string, r toolresolve.Resolution) (Status, string) {
	switch r.Verdict {
	case toolresolve.VerdictOK:
		return StatusOK, "launcher binary: " + r.Bin

	case toolresolve.VerdictOKOffPath:
		hygiene := ""
		if len(r.Notes) > 0 {
			hygiene = r.Notes[0]
		}
		msg := "launcher binary found off PATH: " + r.Bin
		if hygiene != "" {
			msg += " — " + hygiene
		}
		return StatusWarn, msg

	case toolresolve.VerdictShadowed:
		var shims []string
		for _, s := range r.Shadowing {
			shims = append(shims, s.Path)
		}
		return StatusWarn, "launcher binary shadowed by a Windows interop shim on PATH (" + strings.Join(shims, ", ") +
			") — using the native binary at " + r.Bin + " instead"

	case toolresolve.VerdictForeignOnly:
		msg := "installed on Windows, not in WSL — only a Windows interop install was found"
		if fc := firstForeignCandidatePath(r.Considered); fc != "" {
			msg += " (" + fc + ")"
		}
		msg += "; the daemon cannot launch it. Install natively: " + firstInstallDisplay(r.Installs) +
			"; cross-OS launch is a planned follow-up"
		return StatusWarn, msg

	case toolresolve.VerdictNotFound:
		return StatusWarn, "launcher binary not found — install: " + firstInstallDisplay(r.Installs)

	default:
		return StatusWarn, tool + " launcher resolution: " + string(r.Verdict)
	}
}

// firstInstallDisplay returns the first grounded install hint's Display, or
// the honest vendor-docs fallback when none are grounded.
func firstInstallDisplay(installs []integration.InstallHint) string {
	for _, h := range installs {
		if h.Display != "" {
			return h.Display
		}
	}
	return "no grounded install command — see the vendor's docs"
}

// firstForeignCandidatePath returns the first foreign (Windows-only)
// candidate's path from a resolution's evidence trail, or "" when none was
// recorded.
func firstForeignCandidatePath(considered []toolresolve.Candidate) string {
	for _, c := range considered {
		if c.Foreign {
			return c.Path
		}
	}
	return ""
}

// tokenTierSummary renders an adapter's token/cost capture coverage (the
// registry ledger) for the doctor: the best available tier plus any honest
// known gap. The cost engine is already agnostic; this gauge tracks the
// per-adapter CAPTURE holes the coverage-parity Phase 5 fixes shrink.
func tokenTierSummary(t integration.TokenTier) string {
	best := t.Best
	if best == "" {
		best = "unknown"
	}
	if t.Gap == "" {
		return "best tier=" + best + " (no known gap)"
	}
	return "best tier=" + best + " — known gap: " + t.Gap
}

// vocabularySummary renders an adapter's native tool VOCABULARY coverage
// (the registry ledger) for the doctor.
//
// Three shapes, dispatched on the declared capability (never tool name):
//
//   - InTaxonomy — states the coverage plainly; any Note is a
//     partial-coverage caveat and is appended VERBATIM.
//   - honest zero (InTaxonomy false, Note set) — prints the Note VERBATIM.
//     The Note is the grounded reason this capture has no tool vocabulary;
//     summarizing it here would put a rendering-layer guess in front of the
//     registry's evidence.
//   - undeclared (the zero value) — says exactly that. registry_coverage_
//     test.go rejects this state, so it should be unreachable in a shipped
//     build; rendering still refuses to invent a claim for it.
func vocabularySummary(v integration.Vocabulary) string {
	if v.InTaxonomy {
		s := "in taxonomy (native tool names classified via internal/tooltax)"
		if v.Note != "" {
			s += " — " + v.Note
		}
		return s
	}
	if v.Note != "" {
		return v.Note
	}
	return "not declared in the integration registry"
}

// attachResumeSummary renders an adapter's session attach/resume capability
// (the registry ledger) for the doctor: whether `observer <sub> --attach`
// can hand the tool's PTY to the daemon for dashboard jump-in, and how a
// closed session reopens. It dispatches on capability SHAPE (Attach non-nil,
// Resume.Kind, Handoff.Launchable), never on tool name. The zero value is
// the honest floor: "not attachable" + the handoff-fork resume when a
// launcher exists, else no resume.
func attachResumeSummary(ic integration.Capability) string {
	attach := "not attachable (bare launch only)"
	if ic.Attach != nil {
		attach = "attachable via `observer " + ic.Attach.Subcommand + " --attach`"
	}
	var resume string
	switch {
	case ic.Resume.Kind == integration.ResumeNative:
		resume = "native resume via `observer " + ic.Resume.Subcommand + " --resume <id>`"
	case ic.Handoff.Launchable():
		resume = "handoff-fork resume (--continue-from)"
	default:
		resume = "no resume (file-lane carry only)"
	}
	return attach + "; " + resume
}

// nativeRailsSummary renders an adapter's native-console rails (the
// registry ledger) as a one-line human summary for the doctor. A vendor
// with no rails is enrollment-only — correct for most adapters, not a gap.
func nativeRailsSummary(n integration.NativeRails) string {
	if !n.Any() {
		return "enrollment-only (no vendor telemetry rails)"
	}
	var rails []string
	if n.A {
		rails = append(rails, "A:node-telemetry")
	}
	if n.B {
		rails = append(rails, "B:managed-config")
	}
	if n.C {
		rails = append(rails, "C:org-analytics")
	}
	s := "rails " + strings.Join(rails, " ")
	if n.Note != "" {
		s += " (" + n.Note + ")"
	}
	return s
}

// adapterDetected reports whether any of an adapter's watch directories
// exists on this host (a proxy for "this tool is installed + has run"). A
// package-level var (like launchResolve) so tests can force a deterministic
// detected/undetected split without depending on the real host filesystem.
var adapterDetected = func(a adapter.Adapter) bool {
	for _, p := range a.WatchPaths() {
		if dirExists(p) {
			return true
		}
	}
	return false
}

// enabledSet returns the enabled-adapters allow-list as a set, or nil
// when no explicit list is configured (nil == every adapter enabled).
func enabledSet(cfg config.Config) map[string]bool {
	list := cfg.Observer.Watch.EnabledAdapters
	if len(list) == 0 {
		return nil
	}
	m := make(map[string]bool, len(list))
	for _, n := range list {
		m[n] = true
	}
	return m
}

// azureProviderConfigured reports whether the environment carries a
// signal that an Azure OpenAI / AI Foundry provider is in use — the
// case where an OpenAI-compatible tool may bypass its base-URL env var.
func azureProviderConfigured() bool {
	for _, k := range []string{"AZURE_RESOURCE_NAME", "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_KEY", "AZURE_API_KEY"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OPENAI_API_TYPE")), "azure")
}
