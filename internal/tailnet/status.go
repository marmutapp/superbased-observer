package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"os/user"
	"regexp"
	"strings"
	"time"
)

// execTimeout bounds every READ tailscale CLI invocation. The commands are local
// and fast; a hung tailscaled must never block a dashboard request.
const execTimeout = 3 * time.Second

// serveTimeout bounds `tailscale serve` (a config-apply, slightly slower than a
// read, and slower still the first time the control plane is contacted).
const serveTimeout = 20 * time.Second

// serveEnableURLRe extracts the Tailscale admin-consent URL that `tailscale
// serve` prints when Serve is not yet enabled on the tailnet — the ONE step
// Observer cannot perform for the user (it is a control-plane consent in the
// user's Tailscale account). Example:
//
//	https://login.tailscale.com/f/serve?node=nsudg7LtHj11CNTRL
var serveEnableURLRe = regexp.MustCompile(`https://login\.tailscale\.com/\S*serve\S*`)

// servePrivilegeRe recognizes the failure the non-root daemon hits when it
// tries to apply a serve config without the operator grant / root. Tailscale
// prints "sending serve config: Access denied: serve config denied"; other
// privilege phrasings ("permission denied") are matched for robustness. The
// actionable fix is the one-time operator grant (OperatorGrantArgv), NOT an
// error to surface raw — so RunServe flags NeedsPrivilege and the dashboard
// offers to run the grant in the in-dashboard terminal.
var servePrivilegeRe = regexp.MustCompile(`(?i)access denied|serve config denied|permission denied`)

// usernameRe bounds the daemon username interpolated into the operator-grant
// argv (defence-in-depth even though the value comes from os/user, never a
// client). A tailnet/unix login name is ASCII word-ish.
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ServeResult is the outcome of RunServe.
type ServeResult struct {
	// OK is true when serve was applied (or was already applied — idempotent).
	OK bool
	// EnableURL is set when the tailnet must first enable Serve: the user opens
	// it once, approves, and re-runs. Not automatable (control-plane consent).
	EnableURL string
	// NeedsPrivilege is true when serve failed because the non-root daemon
	// lacks the operator grant (or root). The remedy is the one-time
	// OperatorGrantArgv, which the dashboard runs in the in-dashboard terminal
	// — not a raw error. Distinct from EnableURL (a control-plane consent).
	NeedsPrivilege bool
	// Output is the trimmed combined CLI output for honest display/logging.
	// Serve carries no secret, so surfacing it is safe.
	Output string
	// Err is a short human message when serve failed for a reason other than
	// the enable gate.
	Err string
}

// RunServe execs `tailscale serve --bg <port>` on the user's behalf (the plan's
// §D "copy-and-run" step, automated per operator direction 2026-07-13). port is
// the loopback backend port; a leading colon is tolerated and stripped, and the
// result MUST be a bare numeric port (defence-in-depth against exec injection,
// even though the value originates from Observer's own config). serve is
// idempotent — re-running for the same port is a no-op. When Serve is not yet
// enabled on the tailnet, tailscale prints an admin-consent URL, which RunServe
// returns as EnableURL so the dashboard can one-click it rather than fail.
func RunServe(ctx context.Context, port string) ServeResult {
	port = strings.TrimPrefix(strings.TrimSpace(port), ":")
	if !isNumericPort(port) {
		return ServeResult{Err: "invalid backend port"}
	}
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return ServeResult{Err: "tailscale is not installed (no `tailscale` on PATH)"}
	}
	cctx, cancel := context.WithTimeout(ctx, serveTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "serve", "--bg", port).CombinedOutput()
	text := strings.TrimSpace(string(out))
	// The enable gate is the actionable state regardless of exit code — surface
	// its URL first.
	if url := serveEnableURLRe.FindString(text); url != "" {
		return ServeResult{EnableURL: url, Output: text}
	}
	if err != nil {
		if servePrivilegeRe.MatchString(text) {
			return ServeResult{
				NeedsPrivilege: true,
				Err:            "serve needs a one-time permission grant (the daemon runs unprivileged)",
				Output:         text,
			}
		}
		return ServeResult{Err: firstNonEmptyLine(text), Output: text}
	}
	return ServeResult{OK: true, Output: text}
}

// CurrentDaemonUser returns the daemon's own effective login name (from
// os/user, NEVER $USER / a request / the environment — codex 2026-07-13) and
// whether it is root. It is the identity the Tailscale operator grant is
// bound to: after `sudo tailscale set --operator=<name>`, that user (and thus
// the daemon, which runs as it) can apply serve configs without sudo. A
// username that does not match usernameRe is refused rather than interpolated.
func CurrentDaemonUser() (name string, isRoot bool, err error) {
	u, uerr := user.Current()
	if uerr != nil {
		return "", false, fmt.Errorf("tailnet.CurrentDaemonUser: %w", uerr)
	}
	name = strings.TrimSpace(u.Username)
	if !usernameRe.MatchString(name) {
		return "", false, fmt.Errorf("tailnet.CurrentDaemonUser: unexpected username %q", u.Username)
	}
	return name, u.Uid == "0", nil
}

// OperatorGrantArgv is the fully server-derived argv for the one-time Tailscale
// operator grant: `sudo tailscale set --operator=<name>`. name must already be
// validated (CurrentDaemonUser is the only intended source). The dashboard runs
// this in the in-dashboard PTY so the user types their sudo password once;
// afterwards RunServe succeeds unprivileged. argv[0] ("sudo") resolves via PATH.
func OperatorGrantArgv(name string) []string {
	return []string{"sudo", "tailscale", "set", "--operator=" + name}
}

// LoginArgv is the fully server-derived argv for the interactive Tailscale
// login (`tailscale up`). The dashboard runs it in the in-dashboard PTY so the
// auth URL `tailscale up` prints is shown right there and the user opens it on
// their phone/browser. There is NO client input — the only decision is whether
// to prefix `sudo`, resolved server-side from the daemon identity (isRoot,
// sourced from CurrentDaemonUser, never a request):
//   - root daemon → ["tailscale", "up"] (no sudo needed).
//   - non-root daemon → ["sudo", "tailscale", "up"].
//
// A user who has already been granted Tailscale operator rights COULD run
// `tailscale up` unprivileged, but that grant is not guaranteed on a fresh
// host, so sudo is the reliable default for the non-root daemon; the sudo
// password is typed interactively in the xterm (same UX as OperatorGrantArgv),
// never stored or handled by the daemon. argv[0] resolves via PATH.
func LoginArgv(isRoot bool) []string {
	if isRoot {
		return []string{"tailscale", "up"}
	}
	return []string{"sudo", "tailscale", "up"}
}

// InstallArgv is the fully server-derived argv for installing Tailscale on
// Linux via the official install script: `sudo sh -c 'curl -fsSL --proto
// '=https' --tlsv1.2 https://tailscale.com/install.sh | sh'`. It is a FIXED
// closed enum — no client input reaches it. The dashboard runs it in the
// in-dashboard PTY (Linux only; the handler refuses on other OSes and when
// tailscale is already present) so the sudo password is typed interactively,
// never stored. argv[0] ("sudo") resolves via PATH; the piped curl|sh is the
// vendor-documented install path (linked in the UI so the user can inspect it
// first).
//
// Security posture (accepted residual, adversarial review 2026-07-16): this is
// Tailscale's official, operator-initiated install method over a FIXED literal
// HTTPS URL behind an explicit confirm token, Local-route only. The cheap
// hardening applied is `--proto '=https' --tlsv1.2`, which forbids curl from
// following a redirect down to a plaintext/other-protocol scheme and floors the
// TLS version. The heavier download-to-tempfile + digest-pin rewrite is
// DELIBERATELY NOT done: it would fork from vendor guidance, the script content
// is unversioned upstream (no stable digest to pin), and the trust root is
// already the same TLS PKI the pinned digest would be fetched over. See
// docs/security.md.
func InstallArgv() []string {
	return []string{"sudo", "sh", "-c", "curl -fsSL --proto '=https' --tlsv1.2 https://tailscale.com/install.sh | sh"}
}

// isNumericPort reports whether s is a non-empty string of ASCII digits in the
// valid TCP port range.
func isNumericPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	return n >= 1 && n <= 65535
}

// firstNonEmptyLine returns the first non-blank line of s (a compact error for
// the UI), or a generic message when s is empty.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return "tailscale serve failed"
}

// Status is the best-effort result of `tailscale status --json`. Every field
// degrades cleanly: an absent binary yields the zero value (Present=false), a
// logged-out node yields Present=true / LoggedIn=false. The caller renders
// these as first-class states, never as an error.
type Status struct {
	// Present is true when the `tailscale` binary is on PATH.
	Present bool
	// LoggedIn is true when the local node reports BackendState "Running".
	LoggedIn bool
	// Host is Self.DNSName (trailing dot trimmed) — the tailnet HTTPS host the
	// browser reaches the dashboard on. Empty when not logged in.
	Host string
	// State is the raw BackendState string ("Running", "NeedsLogin",
	// "Stopped", …) for honest display.
	State string
}

// tailscaleStatusJSON is the subset of `tailscale status --json` we parse.
type tailscaleStatusJSON struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

// Detect runs `tailscale status --json` and reports presence / login / host.
// It never returns an error: a missing binary, a non-zero exit (logged out),
// or a parse failure all resolve to the honest zero-ish state.
func Detect(ctx context.Context) Status {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return Status{Present: false}
	}
	cctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "status", "--json").Output()
	if err != nil {
		// Binary present but the command failed (commonly: logged out). Report
		// Present=true so the UI distinguishes "not installed" from "not up".
		return Status{Present: true}
	}
	var parsed tailscaleStatusJSON
	if jerr := json.Unmarshal(out, &parsed); jerr != nil {
		return Status{Present: true}
	}
	st := Status{
		Present:  true,
		State:    strings.TrimSpace(parsed.BackendState),
		LoggedIn: strings.EqualFold(strings.TrimSpace(parsed.BackendState), "Running"),
		Host:     strings.TrimSuffix(strings.TrimSpace(parsed.Self.DNSName), "."),
	}
	return st
}

// Host is the convenience the CLI uses: the tailnet HTTPS host, or "" when
// tailscale is not installed / not up. It is Detect(ctx).Host.
func Host(ctx context.Context) string {
	return Detect(ctx).Host
}

// ServeCommand renders the exact `tailscale serve` command that points the
// tailnet HTTPS front end at Observer's loopback backend port. backendPort is
// the ":<port>" form (see remotecfg.BackendPortOnly). It is a STRING for the
// operator to run — Observer never executes `tailscale serve` itself (daemon
// privileged-exec + WSL/Windows tailscaled split; plan §D). Empty backendPort
// yields "" so the caller can hide the affordance until a backend is known.
func ServeCommand(backendPort string) string {
	// Tailscale's target is a BARE port (or host:port / URL) — `tailscale serve
	// --bg 34109`, per `tailscale serve --help`. A colon-prefixed ":34109" is
	// parsed as host="" and fails with "invalid hostname or IP address ''", so
	// strip the leading colon BackendPortOnly carries for net.Listen.
	p := strings.TrimPrefix(strings.TrimSpace(backendPort), ":")
	if p == "" {
		return ""
	}
	return "tailscale serve --bg " + p
}

// ServeStatus reports, best-effort, whether a `tailscale serve` mapping already
// forwards to backendPort. detectable is false when `tailscale serve status`
// cannot be run or parsed — the caller then presents "unknown", never a
// misleading "not configured". It is a substring probe (the serve-status output
// names the forwarded port), so it is intentionally conservative: a true result
// means a mapping to that port was found; a false result with detectable=true
// means none was found.
func ServeStatus(ctx context.Context, backendPort string) (configured, detectable bool) {
	port := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(backendPort), ":"))
	if port == "" {
		return false, false
	}
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return false, false
	}
	cctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "serve", "status").CombinedOutput()
	if err != nil {
		// `tailscale serve status` exits non-zero when nothing is configured on
		// some versions; treat any exec failure as "not detectable" rather than
		// asserting a state.
		if len(out) == 0 {
			return false, false
		}
	}
	text := string(out)
	// The serve-status output lists the forwarded target as "127.0.0.1:<port>"
	// or "http://127.0.0.1:<port>". A bare ":<port>" match is too loose (it can
	// hit an unrelated column), so require the loopback host prefix.
	if strings.Contains(text, "127.0.0.1:"+port) || strings.Contains(text, "localhost:"+port) {
		return true, true
	}
	return false, true
}
