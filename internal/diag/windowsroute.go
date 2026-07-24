package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/proxyroute"
)

// Cross-OS proxy-route doctor check (Phase 7).
//
// When the observer daemon runs in WSL2 but Claude Code / Codex were
// installed from Windows, `observer init` can write the tool's route into
// the Windows-side .claude/.codex pointing it at the WSL proxy over
// localhost NAT forwarding (internal/proxyroute/windows.go). This check
// reports whether those Windows-side routes are written and — best-effort
// — whether the WSL proxy is actually reachable from the Windows side.
//
// It NEVER emits StatusFail: an unreachable probe or a missing route is at
// most a StatusWarn, because the failure modes here are configuration
// mismatches the operator may have deliberately chosen, and the
// reachability probe itself is unverifiable from inside WSL (see below).

const windowsRouteCheckName = "windows proxy routes"

// windowsCurlProbe reports the HTTP status code the Windows-side curl.exe
// gets when hitting target, or an error when curl.exe is absent / the
// request could not complete. Injected so tests never shell out.
type windowsCurlProbe func(ctx context.Context, target string) (int, error)

// windowsRouteTool pairs a detected Windows-side home with the route state
// read out of its config file.
type windowsRouteTool struct {
	label   string // "claude-code-windows" / "codex-windows"
	written bool
	baseURL string
	config  string // the config file path inspected
}

// checkWindowsProxyRoutes is the production entry point. It resolves the
// ambient WSL state, Windows homes, proxy port, the ownership predicate, and
// the real curl.exe probe, then delegates to windowsProxyRoutesCheck. Returns
// ok=false to signal "no row" (Run skips the append) when there is nothing to
// report.
func checkWindowsProxyRoutes(ctx context.Context, cfg config.Config) (Check, bool) {
	return windowsProxyRoutesCheck(ctx, cfg, crossmount.IsWSL(), crossmount.AllHomes(),
		crossmount.HomeOwnedByCurrentWindowsUser, probeWindowsCurl)
}

// windowsProxyRoutesCheck is the injectable core. isWSL / homes / owned / probe
// are passed in so tests exercise every branch without touching the host. owned
// is the crossmount ownership seam: it reports whether a Windows USER home
// belongs to the current Windows user, so ambiguous / unverified cross-OS homes
// can be surfaced with the recovery flag (F2c).
func windowsProxyRoutesCheck(ctx context.Context, cfg config.Config, isWSL bool, homes []crossmount.HomeRoot, owned func(string) bool, probe windowsCurlProbe) (Check, bool) {
	if !isWSL {
		return Check{}, false // not a WSL guest — nothing cross-OS to route
	}
	detected := detectWindowsRouteTools(homes)
	ownershipWarns := windowsHomeOwnershipWarnings(homes, owned)
	if len(detected) == 0 && len(ownershipWarns) == 0 {
		// No Windows-side .claude/.codex/.cursor to route or disambiguate.
		return Check{}, false
	}

	port := cfg.Proxy.Port
	if port <= 0 {
		port = 8820
	}

	status := StatusOK
	var details []string
	anyWritten := false
	for _, d := range detected {
		if d.written {
			anyWritten = true
			details = append(details, fmt.Sprintf("%s: routed in %s → %s", d.label, d.config, d.baseURL))
		} else {
			status = StatusWarn
			details = append(details, fmt.Sprintf("%s: %s present but NOT routed — run `observer init` to point it at the WSL proxy", d.label, d.config))
		}
	}

	// Ownership WARN (F2c): a multi-user Windows machine can expose several
	// /mnt/c/Users/<u>/.claude (or .codex/.cursor) homes, or a single home
	// whose ownership can't be verified against the current Windows user.
	// proxyroute / the hook registrar both refuse to auto-pick in those cases
	// (WindowsRouteTargets / detectWindowsHome EXCLUDE the tool so `observer
	// init` never offers it), so surface the candidates + the exact recovery
	// flag here rather than letting the operator wonder why a detected tool is
	// missing from init. Covers .cursor (hooks-only) too. Never StatusFail.
	for _, w := range ownershipWarns {
		status = StatusWarn
		details = append(details, w)
	}

	// Best-effort reachability: does the Windows side actually reach the WSL
	// proxy over localhost forwarding? Only meaningful once at least one
	// route is written. Never downgrades to StatusFail.
	if anyWritten {
		target := fmt.Sprintf("http://localhost:%d/", port)
		code, err := probe(ctx, target)
		switch {
		case err == nil && code >= 100:
			details = append(details, fmt.Sprintf("reachable from Windows: curl.exe %s → HTTP %d", target, code))
		default:
			// curl.exe missing, timed out, or connection refused — we cannot
			// distinguish "proxy down" from "curl.exe absent" from inside WSL,
			// so stay honest and don't fail.
			details = append(details, "reachability unverifiable from WSL — if Windows tools can't connect, check .wslconfig (mirrored networking / localhostForwarding=false)")
		}
	}

	msg := fmt.Sprintf("%d Windows-side route target(s) detected", len(detected))
	switch {
	case len(detected) == 0:
		msg = fmt.Sprintf("%d Windows-side cross-OS home(s) need disambiguation", len(ownershipWarns))
	case !anyWritten:
		msg = fmt.Sprintf("%d Windows-side tool(s) detected, none routed yet", len(detected))
	}
	return Check{Name: windowsRouteCheckName, Status: status, Message: msg, Details: details}, true
}

// crossOSHomeSpec pairs a cross-OS tool label with its Windows-side config
// subdir and the exact `observer init` recovery flag that disambiguates it.
// cursor is hooks-only (no proxy-route writer) but still needs the ownership
// recovery hint, so it rides the same table (CLAUDE.md #5 — a data table, not a
// switch ladder).
type crossOSHomeSpec struct {
	label  string
	subdir string
	flag   string
}

var crossOSHomeSpecs = []crossOSHomeSpec{
	{"claude-code-windows", ".claude", "observer init --windows-claude-home <path>"},
	{"codex-windows", ".codex", "observer init --windows-codex-home <path>"},
	{"cursor-windows", ".cursor", "observer init --windows-cursor-home <path>"},
}

// windowsHomeOwnershipWarnings walks the Windows-side homes for each cross-OS
// tool's config subdir and returns a WARN line for every tool whose homes can't
// be resolved to a single OWNED home — i.e. several candidates, or a single
// candidate whose ownership the injected `owned` predicate can't verify. The
// resolution rule mirrors proxyroute.resolveWindowsHome / hook.detectWindowsHome
// (exactly one owned home resolves; anything else is refused). owned is injected
// (the crossmount ownership seam) so tests are deterministic without a cmd.exe
// interop shell.
func windowsHomeOwnershipWarnings(homes []crossmount.HomeRoot, owned func(string) bool) []string {
	if owned == nil {
		return nil
	}
	var out []string
	for _, spec := range crossOSHomeSpecs {
		var candidates []string
		ownedCount := 0
		for _, h := range homes {
			if h.OS != crossmount.OSWindows {
				continue
			}
			dir := filepath.Join(h.Path, spec.subdir)
			if !dirExists(dir) {
				continue
			}
			candidates = append(candidates, dir)
			if owned(h.Path) {
				ownedCount++
			}
		}
		switch {
		case len(candidates) == 0:
			continue // nothing detected for this tool
		case ownedCount == 1:
			// Resolves cleanly regardless of how many homes were detected:
			// resolveWindowsHome / the hook registrar auto-pick the single
			// OWNED candidate, so a healthy multi-user machine must not WARN.
			continue
		case len(candidates) == 1:
			out = append(out, fmt.Sprintf(
				"%s: found %s but could not verify it belongs to the current Windows user (%%USERNAME%% mismatch or interop unavailable) — `observer init` will NOT auto-pick it; run `%s` to confirm",
				spec.label, candidates[0], spec.flag,
			))
		default:
			out = append(out, fmt.Sprintf(
				"%s: multiple Windows-side homes carry the config (%s) — `observer init` will NOT auto-pick; run `%s` (the Windows user home, e.g. /mnt/c/Users/<you>) to choose",
				spec.label, strings.Join(candidates, ", "), spec.flag,
			))
		}
	}
	return out
}

// detectWindowsRouteTools walks the crossmount homes for Windows-side
// .claude/.codex dirs and reads each one's route state out of its config
// file. Mirrors proxyroute.WindowsRouteTargets' detection (OS==windows +
// subdir present) but ALSO inspects whether the route is written, which
// the target list alone does not carry.
func detectWindowsRouteTools(homes []crossmount.HomeRoot) []windowsRouteTool {
	var out []windowsRouteTool
	for _, h := range homes {
		if h.OS != crossmount.OSWindows {
			continue
		}
		if claudeDir := filepath.Join(h.Path, ".claude"); dirExists(claudeDir) {
			cfgPath := filepath.Join(claudeDir, "settings.json")
			url, written := claudeWindowsRouteState(cfgPath)
			out = append(out, windowsRouteTool{label: "claude-code-windows", written: written, baseURL: url, config: cfgPath})
		}
		if codexDir := filepath.Join(h.Path, ".codex"); dirExists(codexDir) {
			cfgPath := filepath.Join(codexDir, "config.toml")
			url, written := codexWindowsRouteState(cfgPath)
			out = append(out, windowsRouteTool{label: "codex-windows", written: written, baseURL: url, config: cfgPath})
		}
	}
	return out
}

// claudeWindowsRouteState reports the env.ANTHROPIC_BASE_URL written into a
// Windows-side settings.json and whether it points at an observer proxy.
func claudeWindowsRouteState(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", false
	}
	envRaw, ok := settings["env"]
	if !ok {
		return "", false
	}
	var env map[string]any
	if err := json.Unmarshal(envRaw, &env); err != nil {
		return "", false
	}
	url, _ := env["ANTHROPIC_BASE_URL"].(string)
	return url, proxyroute.IsObserverBaseURL(url)
}

// codexWindowsRouteState reports the observer provider's base_url written
// into a Windows-side config.toml and whether it points at an observer
// proxy with the top-level model_provider switched to it.
func codexWindowsRouteState(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	root := map[string]any{}
	if err := toml.Unmarshal(raw, &root); err != nil {
		return "", false
	}
	providers, _ := root["model_providers"].(map[string]any)
	ours, _ := providers[proxyroute.ProviderName].(map[string]any)
	url, _ := ours["base_url"].(string)
	mp, _ := root["model_provider"].(string)
	return url, proxyroute.IsObserverBaseURL(url) && mp == proxyroute.ProviderName
}

// probeWindowsCurl is the production windowsCurlProbe: it invokes the
// Windows-side curl.exe (reachable from WSL at the fixed System32 path) to
// test whether the WSL proxy answers over localhost forwarding. A missing
// curl.exe or any exec/parse failure returns an error — the caller treats
// that as "unverifiable", never a hard failure.
func probeWindowsCurl(ctx context.Context, target string) (int, error) {
	const curlPath = "/mnt/c/Windows/System32/curl.exe"
	if _, err := os.Stat(curlPath); err != nil {
		return 0, fmt.Errorf("curl.exe unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, curlPath,
		"--max-time", "2", "-s", "-o", "NUL", "-w", "%{http_code}", target).Output()
	if err != nil {
		return 0, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unparseable curl status %q: %w", out, err)
	}
	return code, nil
}
