// codex.go — `observer codex` launcher subcommand.
//
// Codex CLI 0.129.0 supports two auth paths (API key sk-... and
// ChatGPT-Plus subscription JWT) and routes both through `/v1/responses`
// — the API-key form against api.openai.com directly, the JWT form
// against chatgpt.com/backend-api/codex/responses. Both forms can be
// redirected at observer's proxy by overriding the built-in `openai`
// provider's base URL via the `openai_base_url` top-level config field.
//
// The launcher injects `-c openai_base_url='"<proxy>/v1"'` into codex's
// argv so observer's proxy intercepts the request body. The proxy
// detects the auth shape (sk- vs eyJ JWT) and path-translates to
// chatgpt.com when needed (see internal/proxy/provider.go::isChatGPTAuthRequest
// + translateChatGPTPath). Same upstream billing — observer just gets
// to see (and compress) the body.
//
// Distinct from `observer claude`: codex doesn't have an OAuth token
// re-export problem because both auth shapes already ride the standard
// Authorization Bearer header that the proxy intercepts unmodified.
// All we need is to point the base URL at the proxy.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/codexipc"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/jobobject"
)

// newCodexCmd implements `observer codex` — runs the user's `codex`
// binary with `-c openai_base_url='"<proxy>/v1"'` injected so the
// Responses API call lands at the observer proxy.
func newCodexCmd() *cobra.Command {
	var (
		configPath       string
		proxyURL         string
		codexPath        string
		exclusive        bool
		noAppServerCheck bool
		detectOnly       bool
		writeConfig      bool
		verify           bool
	)
	cmd := &cobra.Command{
		Use:   "codex [-- codex-args...]",
		Short: "Launch Codex CLI with traffic routed through the observer proxy",
		Long: "Wraps `codex` with `-c openai_base_url='\"<proxy>/v1\"'` injected\n" +
			"into argv so the Responses API request lands at the observer proxy.\n" +
			"Both auth paths (API-key sk-... and ChatGPT-Plus JWT) flow through\n" +
			"the same override — the proxy detects the bearer shape and routes\n" +
			"to api.openai.com vs chatgpt.com/backend-api/codex/responses\n" +
			"automatically.\n\n" +
			"All arguments after the subcommand are forwarded to codex. Use\n" +
			"`--` to separate observer flags from codex flags:\n" +
			"    observer codex -- exec \"hello world\"\n\n" +
			"Requires a running observer proxy. Start one with `observer start`\n" +
			"or `observer proxy start` first.\n\n" +
			"Shared codex `app-server` processes (e.g., the VS Code Codex\n" +
			"extension or Codex Desktop) can silently intercept `codex exec`\n" +
			"calls via codex's global IPC pipe and bypass the proxy override\n" +
			"(V5-1). Pre- and post-flight checks warn when this happens; pass\n" +
			"`--exclusive` to terminate the shared app-server(s) before exec,\n" +
			"`--detect-only` to inspect without running codex, or\n" +
			"`--no-app-server-check` to silence. See\n" +
			"docs/codex-shared-app-server-gotcha.md.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCodexLauncher(cmd.Context(), codexLauncherOptions{
				configPath:       configPath,
				proxyURL:         proxyURL,
				codexPath:        codexPath,
				codexArgs:        args,
				exclusive:        exclusive,
				noAppServerCheck: noAppServerCheck,
				detectOnly:       detectOnly,
				writeConfig:      writeConfig,
				verify:           verify,
				stderr:           cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&codexPath, "codex-path", "", "Path to the codex binary (default: resolve `codex` on PATH)")
	cmd.Flags().BoolVar(&exclusive, "exclusive", false,
		"Terminate detected shared codex app-servers (e.g., VS Code Codex extension) before exec. Operator-hostile but bounded — see docs/codex-shared-app-server-gotcha.md.")
	cmd.Flags().BoolVar(&noAppServerCheck, "no-app-server-check", false,
		"Skip pre- and post-flight detection of shared codex app-servers. For scripts that have verified the host is clean.")
	cmd.Flags().BoolVar(&detectOnly, "detect-only", false,
		"Run pre-flight detection only and exit. Exit code 1 if any shared app-server is detected, 0 otherwise. Does not run codex.")
	cmd.Flags().BoolVar(&writeConfig, "write-config", false,
		"Auto-fix V6-2: when $CODEX_HOME/config.toml is missing openai_base_url (or points elsewhere), write it pointing at the observer proxy. A .bak file is created next to the original before any mutation. Codex 0.130+ silently drops the wrapper's -c override; until upstream fixes that, the file value is the only thing that works. See docs/proxy-wrappers.md.")
	cmd.Flags().BoolVar(&verify, "verify", false,
		"Run every pre-flight check (proxy reachability + V5-1 shared app-server detection + V6-2 config.toml correctness) and exit. Does NOT launch codex. Exit 0 if every check passes, 1 if any fail. Composes with --write-config to auto-fix V6-2 issues during verify. See docs/proxy-wrappers.md.")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

type codexLauncherOptions struct {
	configPath       string
	proxyURL         string
	codexPath        string
	codexArgs        []string
	exclusive        bool
	noAppServerCheck bool
	detectOnly       bool
	writeConfig      bool
	verify           bool
	stderr           interface{ Write([]byte) (int, error) }
}

// runCodexLauncher resolves the proxy URL, prepares the child argv with
// the openai_base_url override, and execs codex. Exit code is forwarded
// via exitErr (same shape as `observer run`).
//
// Before exec, runs codex `app-server` pre-flight detection (V5-1) and
// either warns (default), terminates (--exclusive), or short-circuits
// to inspection-only (--detect-only). The --no-app-server-check flag
// disables the check entirely. See docs/codex-shared-app-server-gotcha.md.
func runCodexLauncher(ctx context.Context, opts codexLauncherOptions) error {
	if opts.detectOnly && opts.exclusive {
		// SilenceErrors: true on the parent cmd hides returned errors.
		// Print explicitly so the operator sees why the wrapper bailed.
		msg := "observer codex: --detect-only and --exclusive are mutually exclusive (pick inspection OR termination)"
		fmt.Fprintln(opts.stderr, msg)
		return errors.New(msg)
	}

	cfg, err := config.Load(config.LoadOptions{GlobalPath: opts.configPath})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Pre-flight app-server detection. Three modes:
	//   --detect-only           : print summary, exit (0 if empty, 1 if found).
	//   --exclusive             : print intent + terminate + recovery hint.
	//   default (no new flag)   : one-line stderr warning if any detected.
	// --no-app-server-check skips the entire branch.
	var preflight []codexipc.Process
	if !opts.noAppServerCheck {
		procs, derr := codexipc.Detect(ctx)
		if derr != nil {
			// Detection failure is non-fatal — surface so the operator
			// can investigate, then continue with the normal exec path.
			fmt.Fprintf(opts.stderr,
				"observer codex: warning — could not enumerate shared codex app-servers: %v (continuing without pre-flight check)\n",
				derr)
		}
		preflight = procs

		switch {
		case opts.detectOnly:
			return runDetectOnly(opts.stderr, procs)
		case opts.exclusive && len(procs) > 0:
			runExclusiveTermination(ctx, opts.stderr, procs)
		case len(procs) > 0:
			emitPreflightWarning(opts.stderr, procs)
		}
	} else if opts.detectOnly {
		// --detect-only + --no-app-server-check is contradictory. Be
		// kind: print a note and exit 0 instead of erroring out.
		fmt.Fprintln(opts.stderr,
			"observer codex: --detect-only requested but --no-app-server-check also set; detection skipped, exiting clean.")
		return nil
	}

	proxyURL := opts.proxyURL
	if proxyURL == "" {
		port := cfg.Proxy.Port
		if port <= 0 {
			port = 8820
		}
		proxyURL = "http://127.0.0.1:" + strconv.Itoa(port)
	}

	// V6-2 pre-flight: warn if $CODEX_HOME/config.toml is missing
	// `openai_base_url` matching the proxy. Codex 0.130+ silently
	// drops the wrapper's argv-injected -c override (see
	// docs/observer-platform-issues-v6.md §V6-2); without the
	// config.toml value, every turn bypasses the proxy. Honors
	// --no-app-server-check (the universal silence escape hatch).
	//
	// When --write-config is set AND any config is misconfigured, fix
	// it in place (with a .bak backup) before running codex. The
	// warning still prints if a write fails for any root; otherwise
	// the run proceeds with the file value reaching the inner
	// app-server.
	if !opts.noAppServerCheck {
		misconfigs := findCodexConfigMisconfigs(codexHomeRoots(), proxyURL)
		switch {
		case len(misconfigs) == 0:
			// silent happy path
		case opts.writeConfig:
			runWriteCodexConfig(opts.stderr, misconfigs)
			// Re-check so a partial failure still warns the operator.
			if warn := checkCodexConfigTOMLBaseURL(codexHomeRoots(), proxyURL); warn != "" {
				fmt.Fprintln(opts.stderr, warn)
			}
		default:
			if warn := checkCodexConfigTOMLBaseURL(codexHomeRoots(), proxyURL); warn != "" {
				fmt.Fprintln(opts.stderr, warn)
				fmt.Fprintln(opts.stderr,
					"observer codex: re-run with --write-config to auto-fix (creates a .bak before mutating).")
			}
		}
	}

	proxyUp := proxyReachable(proxyURL, 250*time.Millisecond)
	if !proxyUp {
		fmt.Fprintf(opts.stderr,
			"observer codex: warning — proxy not reachable at %s (start it with `observer start`)\n",
			proxyURL)
	}

	// --verify short-circuits after every pre-flight check has run.
	// Composes naturally with --write-config (which already ran above)
	// so an operator can do `observer codex --verify --write-config`
	// in one shot to (a) auto-fix V6-2 and (b) confirm everything's
	// healthy without spending a single token.
	if opts.verify {
		return runCodexVerify(opts.stderr, codexVerifyResult{
			ProxyURL:       proxyURL,
			ProxyReachable: proxyUp,
			PreflightProcs: preflight,
			Misconfigs:     findCodexConfigMisconfigs(codexHomeRoots(), proxyURL),
		})
	}

	bin := opts.codexPath
	if bin == "" {
		resolved, lookErr := exec.LookPath("codex")
		if lookErr != nil {
			return fmt.Errorf("locate codex binary: %w (set --codex-path)", lookErr)
		}
		bin = resolved
	}

	args, info := prepareCodexArgs(opts.codexArgs, proxyURL)
	if info.OverrideAlreadyPresent {
		fmt.Fprintf(opts.stderr,
			"observer codex: routing via existing -c openai_base_url override (no inject; user-provided)\n")
	} else {
		fmt.Fprintf(opts.stderr,
			"observer codex: routing via %s (-c openai_base_url injected; auth shape detected by proxy)\n",
			proxyURL)
	}

	child := exec.Command(bin, args...)
	child.Env = os.Environ()
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	// cmdStart anchors the post-flight rollout-file scan: any
	// rollout-*.jsonl modified at-or-after this stamp is in this run's
	// scope. Recorded BEFORE child.Start() so the file ModTime
	// comparison is monotonic.
	cmdStart := time.Now()
	if err := child.Start(); err != nil {
		return fmt.Errorf("exec codex: start: %w", err)
	}
	// V7-1: on Windows, attach codex.exe to a Job Object with
	// KILL_ON_JOB_CLOSE. If this observer wrapper dies (clean exit,
	// watchdog hammer, SIGKILL), Windows closes our handle and
	// codex.exe terminates automatically — no zombie left writing
	// to rollout-*.jsonl. Non-Windows: no-op stub. Attach failure
	// is non-fatal: log to stderr, continue without protection.
	jobCloser, jobErr := jobobject.AttachProcess(child)
	if jobErr != nil {
		fmt.Fprintf(opts.stderr, "observer codex: jobobject attach failed; V7-1 protection unavailable for this run: %v\n", jobErr)
	} else if jobCloser != nil {
		defer jobCloser.Close()
	}
	runErr := child.Wait()

	// Post-flight V5-1 capture-rate check (silent on success). Skipped
	// when --no-app-server-check is set. Errors are swallowed because
	// the check is diagnostic-only — a stale DB or transient FS hiccup
	// must never fail the wrapper.
	if !opts.noAppServerCheck {
		if warn, _ := validateCaptureRate(ctx, opts.configPath, cmdStart, preflight); warn != "" {
			fmt.Fprintln(opts.stderr, warn)
		}
	}

	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return exitErr(ee.ExitCode())
		}
		return fmt.Errorf("exec codex: %w", runErr)
	}
	return nil
}

// runDetectOnly emits a human-readable summary of the pre-flight scan
// and exits without launching codex. Returns exitErr(1) when any
// shared app-server was detected (CI-gate friendly), nil otherwise.
func runDetectOnly(stderr interface{ Write([]byte) (int, error) }, procs []codexipc.Process) error {
	if len(procs) == 0 {
		fmt.Fprintln(stderr, "observer codex: no shared codex app-servers detected; proxy capture should be reliable.")
		return nil
	}
	fmt.Fprintf(stderr, "observer codex: detected %d shared codex app-server(s) — V5-1 bypass risk:\n", len(procs))
	for _, p := range procs {
		fmt.Fprintf(stderr, "  PID %-6d  %-16s  %s\n", p.PID, p.Source, displayPath(p))
	}
	fmt.Fprintln(stderr, "observer codex: re-run with --exclusive to terminate them before exec, or terminate manually. See docs/codex-shared-app-server-gotcha.md.")
	return exitErr(1)
}

// runExclusiveTermination prints what it's about to kill, calls
// codexipc.Terminate for each, and prints the per-PID outcome and a
// recovery hint. Never returns an error — terminations are
// best-effort, surfaced verbatim, and the wrapper continues into the
// normal exec path regardless.
func runExclusiveTermination(ctx context.Context, stderr interface{ Write([]byte) (int, error) }, procs []codexipc.Process) {
	fmt.Fprintf(stderr, "observer codex: terminating %d shared codex app-server(s) per --exclusive:\n", len(procs))
	for _, p := range procs {
		if err := codexipc.Terminate(ctx, p.PID); err != nil {
			fmt.Fprintf(stderr, "  PID %-6d  %-16s  — failed: %v\n", p.PID, p.Source, err)
			continue
		}
		fmt.Fprintf(stderr, "  PID %-6d  %-16s  — terminated\n", p.PID, p.Source)
	}
	fmt.Fprintln(stderr, "observer codex: re-launch your VS Code Codex extension / Codex Desktop manually after this run.")
}

// emitPreflightWarning prints the single concise one-liner when shared
// app-servers are detected and the operator passed neither --exclusive
// nor --detect-only. Self-contained: names PIDs + sources, suggests
// --exclusive, names --no-app-server-check, links the docs.
func emitPreflightWarning(stderr interface{ Write([]byte) (int, error) }, procs []codexipc.Process) {
	var pidParts []string
	for _, p := range procs {
		pidParts = append(pidParts, fmt.Sprintf("PID %d (%s)", p.PID, p.Source))
	}
	fmt.Fprintf(stderr,
		"observer codex: detected %d shared codex app-server(s) — %s; capture may be incomplete. "+
			"Pass --exclusive to terminate them before this run, --no-app-server-check to silence, "+
			"or see docs/codex-shared-app-server-gotcha.md.\n",
		len(procs), strings.Join(pidParts, ", "))
}

// displayPath returns the most informative path-like string for a
// detected process. Prefers Path; falls back to the first whitespace-
// delimited token of CommandLine when Path is empty (POSIX ps output
// doesn't expose the absolute path separately).
func displayPath(p codexipc.Process) string {
	if p.Path != "" {
		return p.Path
	}
	if p.CommandLine == "" {
		return ""
	}
	if i := strings.IndexAny(p.CommandLine, " \t"); i > 0 {
		return p.CommandLine[:i]
	}
	return p.CommandLine
}

// codexArgsInfo records what the launcher injected into codex's argv.
type codexArgsInfo struct {
	OverrideInjected       bool
	OverrideAlreadyPresent bool // user passed their own -c openai_base_url
}

// prepareCodexArgs prepends `-c openai_base_url='"<proxy>/v1"'` to
// codex's argv, unless the user already supplied an `openai_base_url`
// override (via -c openai_base_url=... OR -c model_provider=... — both
// imply intentional routing). Anything the user explicitly set wins;
// the launcher never overrides explicit state.
//
// The override value is TOML-encoded (a string literal must be wrapped
// in quotes inside the TOML value).
func prepareCodexArgs(parent []string, proxyURL string) ([]string, codexArgsInfo) {
	info := codexArgsInfo{}
	if hasUserCodexConfigOverride(parent) {
		info.OverrideAlreadyPresent = true
		// Pass parent through unchanged. User's intent wins.
		return append([]string{}, parent...), info
	}
	// Strip a trailing slash from proxyURL before appending /v1 so we
	// don't end up with `//v1`.
	base := strings.TrimRight(proxyURL, "/")
	override := "openai_base_url=\"" + base + "/v1\""
	out := make([]string, 0, len(parent)+2)
	out = append(out, "-c", override)
	out = append(out, parent...)
	info.OverrideInjected = true
	return out, info
}

// hasUserCodexConfigOverride detects whether the user passed their own
// `openai_base_url` or `model_provider` override in argv. We respect
// either as "user has set up routing" — don't inject.
//
// Matches both `-c key=value` and `--config key=value` shapes, plus the
// space-separated form `-c key=value` where the override comes as the
// next argv slot (`-c`, `key=value`).
func hasUserCodexConfigOverride(args []string) bool {
	for i, a := range args {
		// Combined forms: -c=key=value / --config=key=value
		// (Cobra-style; codex accepts both per its --help.)
		switch {
		case strings.HasPrefix(a, "-c="):
			if isCodexRoutingOverride(a[len("-c="):]) {
				return true
			}
		case strings.HasPrefix(a, "--config="):
			if isCodexRoutingOverride(a[len("--config="):]) {
				return true
			}
		case a == "-c" || a == "--config":
			if i+1 < len(args) && isCodexRoutingOverride(args[i+1]) {
				return true
			}
		}
	}
	return false
}

// isCodexRoutingOverride returns true when `kv` (a `key=value` blob
// codex parses as TOML) sets a routing-relevant field.
func isCodexRoutingOverride(kv string) bool {
	eq := strings.IndexByte(kv, '=')
	if eq <= 0 {
		return false
	}
	key := strings.TrimSpace(kv[:eq])
	switch key {
	case "openai_base_url", "model_provider":
		return true
	}
	return false
}

// codexVerifyResult captures the pre-flight findings runCodexVerify
// reports. Mirrors claudeVerifyResult — both end with a single
// PASS/FAIL summary line and an exit code reflecting health.
type codexVerifyResult struct {
	ProxyURL       string
	ProxyReachable bool
	PreflightProcs []codexipc.Process
	Misconfigs     []codexConfigMisconfig
}

// runCodexVerify prints PASS/FAIL per pre-flight check and returns
// exitErr(1) when any FAIL'd. PASS-only runs exit 0.
func runCodexVerify(stderr interface{ Write([]byte) (int, error) }, r codexVerifyResult) error {
	failed := 0
	fmt.Fprintln(stderr, "observer codex --verify:")

	if r.ProxyReachable {
		fmt.Fprintf(stderr, "  PASS  proxy reachable at %s\n", r.ProxyURL)
	} else {
		fmt.Fprintf(stderr, "  FAIL  proxy NOT reachable at %s — start `observer start` or `observer proxy start`\n", r.ProxyURL)
		failed++
	}

	if len(r.PreflightProcs) == 0 {
		fmt.Fprintln(stderr, "  PASS  no shared codex app-servers detected (V5-1 bypass risk: clean)")
	} else {
		var labels []string
		for _, p := range r.PreflightProcs {
			labels = append(labels, fmt.Sprintf("PID %d (%s)", p.PID, p.Source))
		}
		fmt.Fprintf(stderr, "  FAIL  V5-1: %d shared codex app-server(s) running — %s. Pass --exclusive to terminate them, or close the VS Code Codex extension / Codex Desktop.\n",
			len(r.PreflightProcs), strings.Join(labels, ", "))
		failed++
	}

	if len(r.Misconfigs) == 0 {
		fmt.Fprintln(stderr, "  PASS  every $CODEX_HOME/config.toml correctly sets openai_base_url (V6-2: clean)")
	} else {
		for _, m := range r.Misconfigs {
			var detail string
			switch m.Status {
			case configTOMLOK:
				detail = fmt.Sprintf("openai_base_url=%q (want %s)", m.CurrentValue, m.WantURL)
			case configTOMLMissingKey:
				detail = "key missing"
			case configTOMLMissingFile:
				detail = "file missing"
			}
			fmt.Fprintf(stderr, "  FAIL  V6-2: %s — %s. Pass --write-config to auto-fix (creates a .bak before mutating).\n", m.ConfigPath, detail)
		}
		failed++
	}

	if failed == 0 {
		fmt.Fprintln(stderr, "observer codex --verify: all checks passed; `observer codex` should capture every turn.")
		return nil
	}
	fmt.Fprintf(stderr, "observer codex --verify: %d check(s) failed — fix above and re-verify.\n", failed)
	return exitErr(1)
}

// proxyReachable + splitHostPortFromURL are reused from claude.go in
// the same package — codex routes through the same proxy as claude.
