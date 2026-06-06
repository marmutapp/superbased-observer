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
			"Requires a running observer proxy. Start one with `observer start`\n" +
			"or `observer proxy start` first.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaudeLauncher(claudeLauncherOptions{
				configPath: configPath,
				proxyURL:   proxyURL,
				claudePath: claudePath,
				claudeArgs: args,
				stderr:     cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&claudePath, "claude-path", "", "Path to the claude binary (default: resolve `claude` on PATH)")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

type claudeLauncherOptions struct {
	configPath string
	proxyURL   string
	claudePath string
	claudeArgs []string
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

	env, info, err := prepareClaudeEnv(os.Environ(), proxyURL, claudeCredentialsPath())
	if err != nil {
		return err
	}

	// Soft-warn if the proxy isn't reachable. Don't block — the user may
	// be starting it in a sibling terminal and we'd rather be useful than
	// pedantic. claude will fail loudly on connection refused anyway.
	if !proxyReachable(proxyURL, 250*time.Millisecond) {
		fmt.Fprintf(opts.stderr,
			"observer claude: warning — proxy not reachable at %s (start it with `observer start`)\n",
			proxyURL)
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
	} else if token, err := readOAuthAccessToken(credentialsPath); err != nil {
		// Non-fatal: API-key users won't have this file. Surface only if
		// the file existed but was malformed (the err signals that).
		info.CredentialsErr = err
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
func readOAuthAccessToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.ClaudeAiOauth.AccessToken, nil
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
