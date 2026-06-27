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
	"time"
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

// resolveToolBin resolves the tool's binary: the explicit --<tool>-path
// override when set, otherwise the binary looked up on PATH. The error names
// the override flag so the operator knows how to recover.
func resolveToolBin(name, override, pathFlag string) (string, error) {
	if override != "" {
		return override, nil
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("locate %s binary: %w (set %s)", name, err, pathFlag)
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

// envLauncherSpec configures a simple base-URL-env launcher (the opencode
// shape). Every field in env is a base-URL or non-secret routing hint —
// NEVER an API key.
type envLauncherSpec struct {
	tool     string            // stderr label, e.g. "cline-cli"
	bin      string            // resolved binary path
	args     []string          // forwarded argv
	proxyURL string            // resolved proxy base URL (for the reachability note)
	env      map[string]string // base-URL (+ non-secret) vars to inject when unset
	stderr   io.Writer
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

	if !proxyReachable(spec.proxyURL, 250*time.Millisecond) {
		fmt.Fprintf(spec.stderr,
			"observer %s: warning — proxy not reachable at %s (start it with `observer start`)\n",
			spec.tool, spec.proxyURL)
	} else if len(applied) > 0 {
		fmt.Fprintf(spec.stderr,
			"observer %s: routing via %s (set %s)\n",
			spec.tool, spec.proxyURL, strings.Join(applied, ", "))
	} else {
		fmt.Fprintf(spec.stderr, "observer %s: routing via %s\n", spec.tool, spec.proxyURL)
	}

	child := exec.Command(spec.bin, spec.args...) //nolint:gosec // user-launched tool, args are theirs
	child.Env = childEnv
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
