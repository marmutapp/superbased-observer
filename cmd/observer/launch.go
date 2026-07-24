// launch.go — shared plumbing for the `observer <tool>` launcher family.
//
// The launchers wrap an AI coding tool so its model traffic flows through
// the observer proxy (accurate tokens + compression + cache tracking). The
// SIMPLE shape — inject one or more base-URL env vars, exec the tool,
// forward its exit code — is factored here and reused by opencode, cline-cli
// and copilot-cli. The two COMPLEX launchers keep their own logic because
// their routing is genuinely different: `observer claude` re-exports a
// Pro/Max OAuth token; `observer codex` injects a `-c openai_base_url`
// override into argv and runs codex app-server pre-flight. Both still reuse
// the lower-level primitives here (resolveProxyURL, proxyReachable).
//
// Implementation rule (CLAUDE.md / docs/audits/notes-on-proxy.md): a
// launcher writes ONLY base-URL fields. It NEVER sets, reads, or moves an
// API key — secret auth must already be in the user's environment.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/toolresolve"
	"github.com/marmutapp/superbased-observer/internal/toolresolve/host"
)

// resolveProxyURL returns the proxy base URL a launcher should route to: the
// explicit --proxy override when set, otherwise http://127.0.0.1:<port>
// (cfgPort, or 8820 when unset/invalid). Shared by every launcher so the
// default-port logic lives in exactly one place.
func resolveProxyURL(cfgPort int, override string) string {
	if override != "" {
		return override
	}
	if cfgPort <= 0 {
		cfgPort = 8820
	}
	return "http://127.0.0.1:" + strconv.Itoa(cfgPort)
}

// resolveEnv returns the toolresolve.Env driving the registry resolution
// ladder. It is memoized per process via sync.OnceValue so the login-shell
// PATH capture and the crossmount home walk happen exactly once no matter how
// many launchers resolve. It is a package var so a test can substitute a
// map-backed fake env (restore it in a defer).
var resolveEnv = sync.OnceValue(func() toolresolve.Env {
	return host.NewEnv(host.Options{})
})

// resolveToolBin resolves a launchable tool's binary through the registry-
// driven ladder, returning the launchable path or an honest, actionable error.
// tool is the integration-registry KEY (e.g. "claude-code", "gemini-cli",
// "kimi-code") — the binary spellings, probe dirs, and grounded install hints
// all come from that row, so callers never name the binary themselves. The
// ladder:
//
//  1. override (the --<tool>-path flag) — trusted verbatim, no checks
//     (unchanged trust semantics).
//  2. [launch.tools.<tool>].path from the operator's config — stat-checked; a
//     set-but-missing path is an error NAMING the config key so the operator
//     can fix it. A config that fails to load falls open to the ladder.
//  3. the toolresolve ladder over the registry Binary row: `ok` launches
//     silently; `ok_off_path` / `shadowed` launch with a one-time honest note
//     on stderr; `foreign_only` / `not_found` return an error whose message IS
//     the FormatVerdict text (the grounded install command, or "installed on
//     Windows, not in WSL").
//  4. no Binary row (defensive — the registry coverage test pins one for every
//     launchable tool) → a bare PATH lookup of the tool name.
//
// pathFlag names the override flag surfaced in the step-4 fallback error.
// stderr receives the launchable-but-imperfect notes; pass io.Discard to stay
// quiet.
func resolveToolBin(tool, override, pathFlag, configPath string, stderr io.Writer) (string, error) {
	if override != "" {
		return override, nil
	}

	// Step 2: operator config override. Fail open on a config load error so a
	// broken config never blocks the ladder.
	if cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath}); err == nil {
		if tc, ok := cfg.Launch.Tools[tool]; ok && tc.Path != "" {
			if fi, statErr := os.Stat(tc.Path); statErr != nil || fi.IsDir() {
				return "", fmt.Errorf(
					"%s binary not found at [launch.tools.%s].path = %q — fix the path or remove the entry",
					tool, tool, tc.Path,
				)
			}
			return tc.Path, nil
		}
	}

	// Step 3: registry-driven resolution ladder over the Binary row.
	if row, ok := integration.For(tool); ok && row.Binary != nil {
		r := toolresolve.Resolve(*row.Binary, resolveEnv())
		switch r.Verdict {
		case toolresolve.VerdictOK:
			return r.Bin, nil
		case toolresolve.VerdictOKOffPath, toolresolve.VerdictShadowed:
			fmt.Fprint(stderr, toolresolve.FormatVerdict(tool, pathFlag, r))
			return r.Bin, nil
		default: // foreign_only / not_found
			return "", errors.New(strings.TrimRight(toolresolve.FormatVerdict(tool, pathFlag, r), "\n"))
		}
	}

	// Step 4: defensive fallback — no grounded Binary row (should not happen).
	resolved, err := exec.LookPath(tool)
	if err != nil {
		return "", fmt.Errorf("locate %s binary: %w (set %s)", tool, err, pathFlag)
	}
	return resolved, nil
}

// applyBaseURLEnv injects each key=value in inject into a copy of parent,
// UNLESS the user already exported a non-empty value for that key — theirs
// always wins (a launcher never clobbers explicit env state). Existing
// entries keep their order and value; newly-injected keys are appended in
// sorted order for determinism. Returns the merged env, the keys actually
// injected (applied), and the keys kept from the user (presets).
//
// It is the single env seam for every base-URL launcher: pure, no I/O, fully
// table-testable. Callers must only ever pass base-URL (and non-secret)
// fields here — never an API key.
func applyBaseURLEnv(parent []string, inject map[string]string) (env, applied, presets []string) {
	existing := make(map[string]string, len(parent))
	for _, kv := range parent {
		if i := strings.IndexByte(kv, '='); i > 0 {
			existing[kv[:i]] = kv[i+1:]
		}
	}

	out := make([]string, len(parent))
	copy(out, parent)

	toAppend := make([]string, 0, len(inject))
	for k, v := range inject {
		if cur, ok := existing[k]; ok && cur != "" {
			presets = append(presets, k)
			continue
		}
		toAppend = append(toAppend, k)
		_ = v
	}
	sort.Strings(toAppend)
	for _, k := range toAppend {
		out = append(out, k+"="+inject[k])
		applied = append(applied, k)
	}
	sort.Strings(presets)
	return out, applied, presets
}

// envValue returns the value of key in an env slice ("KEY=value" entries),
// or "" if absent. When a key appears more than once the last wins (exec
// semantics).
func envValue(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[:i] == key {
			val = kv[i+1:]
		}
	}
	return val
}

// lookupEnvValue returns the value of key in an env slice ("KEY=value" entries)
// AND whether the key is PRESENT at all — the os.LookupEnv distinction envValue
// cannot make. It lets a profile-forwarding helper distinguish an absent key
// (forward nothing, the daemon's inherited value stands) from one explicitly set
// to empty (`KEY=`, forward `KEY=` so the child resets to the tool's DEFAULT
// profile) — F3. When a key appears more than once the last wins (exec
// semantics), matching how the child resolves duplicate env entries.
func lookupEnvValue(env []string, key string) (string, bool) {
	val := ""
	found := false
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[:i] == key {
			val = kv[i+1:]
			found = true
		}
	}
	return val, found
}

// envLauncherSpec configures a simple base-URL-env launcher (the opencode
// shape). Every field in env is a base-URL or non-secret routing hint —
// NEVER an API key.
type envLauncherSpec struct {
	tool     string            // stderr label, e.g. "cline-cli"
	bin      string            // resolved binary path
	args     []string          // forwarded argv
	proxyURL string            // resolved proxy base URL (for the reachability note)
	env      map[string]string // base-URL (+ non-secret) vars to inject when unset
	// dir is the child's working directory. Empty inherits the caller's
	// cwd (the default). A `--continue-from` launch sets it to the source
	// session's translated project root (via launchDir) so a cross-OS
	// continuation lands in the real project folder, not the daemon's cwd.
	dir    string
	stderr io.Writer
}

// runEnvLauncher injects the spec's base-URL env vars (user values win),
// prints a one-line routing/preset/unreachable note, then execs the tool and
// forwards its exit code (same shape as `observer run`). Pure exec — it does
// not consult or set any secret.
func runEnvLauncher(spec envLauncherSpec) error {
	childEnv, applied, presets := applyBaseURLEnv(os.Environ(), spec.env)

	for _, k := range presets {
		fmt.Fprintf(spec.stderr,
			"observer %s: %s already set in env; using yours.\n", spec.tool, k)
	}

	switch {
	case !proxyReachable(spec.proxyURL, 250*time.Millisecond):
		fmt.Fprintf(spec.stderr,
			"observer %s: warning — proxy not reachable at %s (start it with `observer start`)\n",
			spec.tool, spec.proxyURL)
	case len(applied) > 0:
		fmt.Fprintf(spec.stderr,
			"observer %s: routing via %s (set %s)\n",
			spec.tool, spec.proxyURL, strings.Join(applied, ", "))
	default:
		fmt.Fprintf(spec.stderr, "observer %s: routing via %s\n", spec.tool, spec.proxyURL)
	}

	child := exec.Command(spec.bin, spec.args...) //nolint:gosec // user-launched tool, args are theirs
	child.Env = childEnv
	child.Dir = spec.dir // "" inherits the caller's cwd (default)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitErr(ee.ExitCode())
		}
		return fmt.Errorf("exec %s: %w", spec.tool, err)
	}
	return nil
}
