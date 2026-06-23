// claude.go — `observer claude` launcher subcommand.
//
// Pro/Max OAuth-authenticated Claude Code (2.1+) reads
// `~/.claude/.credentials.json` and bypasses ANTHROPIC_BASE_URL for the
// `/v1/messages` chat call — sending Bearer tokens straight to
// api.anthropic.com. That ducks the observer proxy, so compression
// never runs and api_turns rows never land for the OAuth majority.
//
// The launcher works around it by re-exporting the OAuth access token
// as ANTHROPIC_AUTH_TOKEN before exec'ing claude. When the SDK sees an
// auth token in the env, it falls back to the regular API-key code
// path, which DOES respect ANTHROPIC_BASE_URL. Same Bearer header on
// the wire, same Pro/Max billing — observer just gets to see (and
// compress) the body.
//
// API-key users (no `~/.claude/.credentials.json`) get the same
// treatment minus the token export: just ANTHROPIC_BASE_URL set, claude
// uses ANTHROPIC_API_KEY as today.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newClaudeCmd implements `observer claude` — sets ANTHROPIC_BASE_URL
// and (when a Pro/Max OAuth token is present) ANTHROPIC_AUTH_TOKEN, then
// execs the user's `claude` binary so its chat traffic flows through
// the observer proxy.
func newClaudeCmd() *cobra.Command {
	var (
		configPath string
		proxyURL   string
		claudePath string
		verify     bool
	)
	cmd := &cobra.Command{
		Use:   "claude [-- claude-args...]",
		Short: "Launch Claude Code with traffic routed through the observer proxy",
		Long: "Wraps `claude` with ANTHROPIC_BASE_URL pointed at the observer\n" +
			"proxy. For Pro/Max OAuth users, also re-exports the OAuth access\n" +
			"token as ANTHROPIC_AUTH_TOKEN so Claude Code's normal `/v1/messages`\n" +
			"path lands at the proxy instead of bypassing it. Same Pro/Max\n" +
			"billing — observer just gets to see (and compress) the body.\n\n" +
			"All arguments after the subcommand are forwarded to claude. Use\n" +
			"`--` to separate observer flags from claude flags:\n" +
			"    observer claude -- --print \"hi\"\n\n" +
			"Pass --verify to run every pre-flight check (proxy reachable,\n" +
			"credentials.json present + parseable, OAuth token discoverable)\n" +
			"and print a one-line PASS/FAIL summary without launching claude.\n\n" +
			"Requires a running observer proxy. Start one with `observer start`\n" +
			"or `observer proxy start` first.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaudeLauncher(claudeLauncherOptions{
				configPath: configPath,
				proxyURL:   proxyURL,
				claudePath: claudePath,
				claudeArgs: args,
				verify:     verify,
				stderr:     cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&claudePath, "claude-path", "", "Path to the claude binary (default: resolve `claude` on PATH)")
	cmd.Flags().BoolVar(&verify, "verify", false, "Run pre-flight checks (proxy reachability + credentials.json + OAuth token) and exit. Does NOT launch claude. Exit 0 if every check passes, 1 if any fail. See docs/proxy-wrappers.md.")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

type claudeLauncherOptions struct {
	configPath string
	proxyURL   string
	claudePath string
	claudeArgs []string
	verify     bool
	stderr     interface{ Write([]byte) (int, error) }
}

// runClaudeLauncher resolves the proxy URL, prepares the child env, and
// execs claude with the original argv. Exit code is forwarded via
// exitErr (same shape as `observer run`).
func runClaudeLauncher(opts claudeLauncherOptions) error {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: opts.configPath})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	proxyURL := opts.proxyURL
	if proxyURL == "" {
		port := cfg.Proxy.Port
		if port <= 0 {
			port = 8820
		}
		proxyURL = "http://127.0.0.1:" + strconv.Itoa(port)
	}

	bin := opts.claudePath
	if bin == "" {
		resolved, lookErr := exec.LookPath("claude")
		if lookErr != nil {
			return fmt.Errorf("locate claude binary: %w (set --claude-path)", lookErr)
		}
		bin = resolved
	}

	credPath := claudeCredentialsPath()
	env, info, err := prepareClaudeEnv(os.Environ(), proxyURL, credPath)
	if err != nil {
		return err
	}

	// Surface a clear stderr line for each credentials-file edge case so
	// the OAuth-fallback path is observable. The wrapper already does
	// the right thing on each branch (file-missing → API-key mode;
	// malformed → API-key mode + surface the parse error); the operator
	// needs to know which path was taken so a silent "API-key mode" line
	// doesn't hide a Pro/Max user who thought OAuth was wiring up.
	switch {
	case info.OAuthStale:
		fmt.Fprintf(opts.stderr,
			"observer claude: stored OAuth token in %s is expired — NOT re-exporting it (a stale env token blocks Claude Code's own refresh and the session 401s). Launching with ANTHROPIC_BASE_URL only; claude refreshes its token itself.\n",
			credPath)
	case info.CredentialsErr != nil:
		fmt.Fprintf(opts.stderr,
			"observer claude: warning — credentials file %s exists but is unparseable (%v); falling back to API-key mode. Fix the file or set ANTHROPIC_AUTH_TOKEN manually if you're a Pro/Max user.\n",
			credPath, info.CredentialsErr)
	case info.OAuthPreset:
		fmt.Fprintf(opts.stderr,
			"observer claude: ANTHROPIC_AUTH_TOKEN already in env; using yours (credentials.json untouched).\n")
	case !info.OAuthInjected:
		// Distinguish "no credentials file at all" (API-key user) from
		// "credentials file present but no token field" (a stale or
		// hand-edited file the user probably wants to know about).
		if credentialsFileExists(credPath) {
			fmt.Fprintf(opts.stderr,
				"observer claude: warning — credentials file %s has no claudeAiOauth.accessToken; falling back to API-key mode. Re-run `claude` to refresh your OAuth credentials if you're a Pro/Max user.\n",
				credPath)
		}
	}

	proxyUp := proxyReachable(proxyURL, 250*time.Millisecond)
	if !proxyUp {
		fmt.Fprintf(opts.stderr,
			"observer claude: warning — proxy not reachable at %s (start it with `observer start`)\n",
			proxyURL)
	}

	if opts.verify {
		return runClaudeVerify(opts.stderr, claudeVerifyResult{
			ProxyURL:        proxyURL,
			ProxyReachable:  proxyUp,
			CredentialsPath: credPath,
			Info:            info,
		})
	}

	if info.OAuthInjected {
		fmt.Fprintf(opts.stderr,
			"observer claude: routing via %s (Pro/Max OAuth token re-exported as ANTHROPIC_AUTH_TOKEN)\n",
			proxyURL)
	} else {
		fmt.Fprintf(opts.stderr,
			"observer claude: routing via %s (ANTHROPIC_BASE_URL only — using existing API-key auth)\n",
			proxyURL)
	}

	child := exec.Command(bin, opts.claudeArgs...)
	child.Env = env
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitErr(ee.ExitCode())
		}
		return fmt.Errorf("exec claude: %w", err)
	}
	return nil
}

// claudeEnvInfo records what the launcher actually changed, so callers
// (and tests) can verify intent without diff'ing two []string slices.
type claudeEnvInfo struct {
	BaseURLSet     bool
	BaseURLPreset  bool // user already had ANTHROPIC_BASE_URL set; we kept theirs
	OAuthInjected  bool
	OAuthPreset    bool  // user already had ANTHROPIC_AUTH_TOKEN; we kept theirs
	OAuthStale     bool  // stored token expired — deliberately NOT re-exported (D13)
	CredentialsErr error // non-fatal — file missing / unreadable / wrong shape
}

// prepareClaudeEnv merges the OAuth-routing env vars into the parent
// environment without clobbering anything the user explicitly set.
//
// Rules:
//   - If ANTHROPIC_BASE_URL is unset, set it to proxyURL.
//   - If ANTHROPIC_AUTH_TOKEN is unset and credentialsPath has a usable
//     `claudeAiOauth.accessToken`, set it from there.
//   - Anything the user already exported wins. The launcher never
//     overrides explicit env state.
func prepareClaudeEnv(parent []string, proxyURL, credentialsPath string) ([]string, claudeEnvInfo, error) {
	env := make(map[string]string, len(parent))
	keys := make([]string, 0, len(parent)) // preserve order for determinism
	for _, kv := range parent {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		k, v := kv[:idx], kv[idx+1:]
		if _, seen := env[k]; !seen {
			keys = append(keys, k)
		}
		env[k] = v
	}

	info := claudeEnvInfo{}

	if existing, ok := env["ANTHROPIC_BASE_URL"]; ok && existing != "" {
		info.BaseURLPreset = true
	} else {
		env["ANTHROPIC_BASE_URL"] = proxyURL
		keys = appendIfMissing(keys, "ANTHROPIC_BASE_URL")
		info.BaseURLSet = true
	}

	if existing, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok && existing != "" {
		info.OAuthPreset = true
	} else if token, stale, err := readOAuthAccessToken(credentialsPath); err != nil {
		// Non-fatal: API-key users won't have this file. Surface only if
		// the file existed but was malformed (the err signals that).
		info.CredentialsErr = err
	} else if stale {
		info.OAuthStale = true
	} else if token != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = token
		keys = appendIfMissing(keys, "ANTHROPIC_AUTH_TOKEN")
		info.OAuthInjected = true
	}

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, info, nil
}

func appendIfMissing(keys []string, k string) []string {
	for _, existing := range keys {
		if existing == k {
			return keys
		}
	}
	return append(keys, k)
}

// claudeCredentialsPath resolves where Claude Code's `.credentials.json`
// lives, honoring CLAUDE_CONFIG_DIR / ANTHROPIC_CONFIG_DIR overrides
// before falling back to ~/.claude/. Matches the binary's own lookup
// order (both env-var names appear in the 2.1.x strings table).
func claudeCredentialsPath() string {
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR"} {
		if dir := os.Getenv(env); dir != "" {
			return filepath.Join(dir, ".credentials.json")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// readOAuthAccessToken returns claudeAiOauth.accessToken from path, or
// "" if the file is missing. Returns a non-nil error only when the file
// exists but can't be parsed as the expected JSON shape — those are
// worth surfacing to the user.
func readOAuthAccessToken(path string) (token string, stale bool, err error) {
	if path == "" {
		return "", false, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			// ExpiresAt is Claude Code's token expiry, ms since epoch.
			// Zero/absent = unknown — treated as fresh (legacy shape).
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	tok := doc.ClaudeAiOauth.AccessToken
	// D13 (2026-06-10): re-exporting an EXPIRED stored token as
	// ANTHROPIC_AUTH_TOKEN suppresses Claude Code's own refresh and the
	// session 401s. Report it stale instead of usable — the launcher
	// then routes via ANTHROPIC_BASE_URL only and claude refreshes
	// itself (capture verified on that path).
	if tok != "" && doc.ClaudeAiOauth.ExpiresAt > 0 &&
		time.Now().UnixMilli() >= doc.ClaudeAiOauth.ExpiresAt {
		return "", true, nil
	}
	return tok, false, nil
}

// credentialsFileExists is a thin wrapper around os.Stat so the wrapper
// can distinguish "no file at all" from "file present but empty or
// missing the token field." Used by stderr-shaping in the OAuth-
// fallback branch.
func credentialsFileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// claudeVerifyResult captures the pre-flight findings runClaudeVerify
// reports. Kept as a struct so future fields (proxy turn capture,
// model availability) plug in without rewriting the call site.
type claudeVerifyResult struct {
	ProxyURL        string
	ProxyReachable  bool
	CredentialsPath string
	Info            claudeEnvInfo
}

// runClaudeVerify prints a PASS/FAIL line per pre-flight check and
// returns exitErr(1) when any FAIL'd. PASS-only runs exit 0. The
// summary mirrors what `observer doctor` would show but scoped to
// just the claude wrapper's contract.
func runClaudeVerify(stderr interface{ Write([]byte) (int, error) }, r claudeVerifyResult) error {
	failed := 0
	fmt.Fprintln(stderr, "observer claude --verify:")

	if r.ProxyReachable {
		fmt.Fprintf(stderr, "  PASS  proxy reachable at %s\n", r.ProxyURL)
	} else {
		fmt.Fprintf(stderr, "  FAIL  proxy NOT reachable at %s — start `observer start` or `observer proxy start`\n", r.ProxyURL)
		failed++
	}

	switch {
	case r.Info.CredentialsErr != nil:
		fmt.Fprintf(stderr, "  FAIL  credentials file %s unparseable: %v\n", r.CredentialsPath, r.Info.CredentialsErr)
		failed++
	case r.Info.OAuthPreset:
		fmt.Fprintln(stderr, "  PASS  ANTHROPIC_AUTH_TOKEN already in env; OAuth credentials file not consulted")
	case r.Info.OAuthInjected:
		fmt.Fprintf(stderr, "  PASS  Pro/Max OAuth token found in %s; would re-export as ANTHROPIC_AUTH_TOKEN\n", r.CredentialsPath)
	case credentialsFileExists(r.CredentialsPath):
		fmt.Fprintf(stderr, "  WARN  %s present but has no accessToken — API-key mode will be used. If you're a Pro/Max user, re-run `claude` interactively to refresh credentials.\n", r.CredentialsPath)
	default:
		fmt.Fprintf(stderr, "  PASS  no Pro/Max credentials file at %s (assuming API-key mode; ensure ANTHROPIC_API_KEY is set)\n", r.CredentialsPath)
	}

	if r.Info.BaseURLPreset {
		fmt.Fprintln(stderr, "  WARN  ANTHROPIC_BASE_URL already set in env; the wrapper would NOT override it. Unset it to route through the observer proxy.")
	} else {
		fmt.Fprintf(stderr, "  PASS  would set ANTHROPIC_BASE_URL=%s\n", r.ProxyURL)
	}

	if failed == 0 {
		fmt.Fprintln(stderr, "observer claude --verify: all checks passed; `observer claude` should capture every turn.")
		return nil
	}
	fmt.Fprintf(stderr, "observer claude --verify: %d check(s) failed — fix above and re-verify.\n", failed)
	return exitErr(1)
}

// proxyReachable returns true when a TCP dial against the proxy URL's
// host:port succeeds within timeout. Used as a soft pre-flight before
// exec — failure is a stderr warning, not a fatal error.
func proxyReachable(proxyURL string, timeout time.Duration) bool {
	host, port, ok := splitHostPortFromURL(proxyURL)
	if !ok {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func splitHostPortFromURL(raw string) (host, port string, ok bool) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "http://"):
		s = s[len("http://"):]
	case strings.HasPrefix(s, "https://"):
		s = s[len("https://"):]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", false
	}
	return host, port, true
}
