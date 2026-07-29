package integration_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	adapterdefaults "github.com/marmutapp/superbased-observer/internal/adapter/defaults"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestRegistryCoversEveryRegisteredAdapter pins that every adapter in the
// canonical defaults.Adapters() list has an integration.Capability row.
// This is the guardrail the new-adapter checklist relies on: add an adapter
// without a registry row and this test goes red, forcing the capability
// declaration that init/register/MCP/doctor iterate. Lives in an external
// test package so it can import adapterdefaults without coupling the pure
// integration package to it.
func TestRegistryCoversEveryRegisteredAdapter(t *testing.T) {
	for _, a := range adapterdefaults.Adapters() {
		name := a.Name()
		if _, ok := integration.For(name); !ok {
			t.Errorf("adapter %q has no integration.Capability row — add one in internal/integration", name)
		}
	}
}

// TestRegistryHasNoOrphanRows pins the reverse: every registry row maps to a
// registered adapter (no stale rows for removed adapters).
//
// A multi-phase adapter build may temporarily land a registry row ahead of
// its parser package + defaults.Adapters() entry; the sanctioned way to do
// that is a documented, dated exception map here that is DELETED the moment
// the package lands. The 2026-07-29 wave (droid / open-interpreter /
// command-code) used one and it was retired when those three registered —
// so this test currently guards the general case with no exceptions, which
// is the state it should normally be in.
func TestRegistryHasNoOrphanRows(t *testing.T) {
	registered := map[string]bool{}
	for _, a := range adapterdefaults.Adapters() {
		registered[a.Name()] = true
	}
	for _, c := range integration.Capabilities() {
		if !registered[c.Tool] {
			t.Errorf("registry row %q has no registered adapter — remove the stale row", c.Tool)
		}
	}
}

// transcriptReader mirrors the handoffsvc TranscriptReader seam locally so
// this test can pin reader implementations without importing the boundary
// package.
type transcriptReader interface {
	ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error)
}

// TestHandoffClassifiedForEveryAdapter is the session-handoff sibling of
// the routability pin: every adapter's Handoff row must expose at least
// the universal file lane, and every adapter that SHIPS a transcript
// reader must be classified TranscriptFull (a reader on an actions-only
// row is a classification bug; a Full row without a reader is fine — the
// P2 tranche ships readers incrementally).
func TestHandoffClassifiedForEveryAdapter(t *testing.T) {
	for _, a := range adapterdefaults.Adapters() {
		cap, _ := integration.For(a.Name())
		if len(cap.Handoff.Lanes()) == 0 {
			t.Errorf("adapter %q: Handoff.Lanes() must include at least the file lane", a.Name())
		}
		if _, ok := a.(transcriptReader); ok && cap.Handoff.Transcript != integration.TranscriptFull {
			t.Errorf("adapter %q implements ReadTranscript but its Handoff row is %q, not full", a.Name(), cap.Handoff.Transcript)
		}
	}
}

// TestLaunchableImpliesInjectPrompt pins the launch capability's internal
// consistency: any adapter declaring a LaunchSpec (startable in the
// dashboard's embedded web terminal) must (a) name a launcher Subcommand
// and (b) also expose the InjectPrompt delivery lane — the launch path
// spawns `observer <Subcommand> --continue-from <id>`, which IS the
// inject_prompt lane, so a Launch row without InjectPrompt would be an
// incoherent capability. The cmd-side sync test
// (cmd/observer) pins Subcommand against the actual wired launcher.
func TestLaunchableImpliesInjectPrompt(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if !c.Handoff.Launchable() {
			continue
		}
		if c.Handoff.Launch.Subcommand == "" {
			t.Errorf("adapter %q: Launch set but Subcommand empty", c.Tool)
		}
		// A DocAssisted launcher opens the TUI + writes the doc (file lane);
		// it injects no prompt, so it needs only the universal InjectFile
		// lane. A Seeded launcher DOES inject the handover as the first
		// prompt, so it must declare the InjectPrompt lane.
		want := integration.InjectPrompt
		if c.Handoff.Launch.Mode == integration.LaunchDocAssisted {
			want = integration.InjectFile
		}
		hasLane := false
		for _, l := range c.Handoff.Lanes() {
			if l == want {
				hasLane = true
				break
			}
		}
		if !hasLane {
			t.Errorf("adapter %q: Launch (mode %d) set but Handoff lacks the %q lane", c.Tool, c.Handoff.Launch.Mode, want)
		}
	}
}

// TestAttachImpliesLauncher pins the session-attach capability's internal
// consistency: any adapter declaring an AttachSpec (its PTY can be handed
// to the daemon via `observer <Subcommand> --attach`) must (a) name a
// non-empty Subcommand and (b) be Launchable — an attach spec is only
// meaningful when a wired launcher exists to spawn the attachable PTY, and
// the launcher IS the Launch capability. It also pins that the attach
// Subcommand matches the launcher's Subcommand, so the two can never drift
// (the cmd-side sync test pins Subcommand against the actual wired
// launcher). An Attach row without a launcher would be an incoherent
// capability.
func TestAttachImpliesLauncher(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if c.Attach == nil {
			continue
		}
		if c.Attach.Subcommand == "" {
			t.Errorf("adapter %q: Attach set but Subcommand empty", c.Tool)
		}
		if !c.Handoff.Launchable() {
			t.Errorf("adapter %q: Attach set but the tool is not Launchable (no wired launcher)", c.Tool)
			continue
		}
		if c.Attach.Subcommand != c.Handoff.Launch.Subcommand {
			t.Errorf("adapter %q: Attach.Subcommand %q != Launch.Subcommand %q (must name the same wired launcher)",
				c.Tool, c.Attach.Subcommand, c.Handoff.Launch.Subcommand)
		}
	}
}

// TestLaunchableImpliesAttach pins the attach-all-launchers invariant
// (2026-07-24): every row with a wired launcher (Handoff.Launch != nil) must
// ALSO declare an AttachSpec whose Subcommand equals the launcher's — because
// every `observer <verb>` launcher now attaches-by-default, so a launchable
// tool without a grounded Attach row would be a launcher the daemon refuses to
// spawn a PTY for (validateAttachCapability). Together with
// TestAttachImpliesLauncher (Attach ⇒ Launchable + verb equality) this makes
// Launch-grounded ⇔ Attach-grounded with equal verbs. Non-launcher rows
// (Handoff.Launch == nil — the *-web rows, legacy IDE adapters, aider, crush,
// cowork) are skipped: nothing to attach to.
func TestLaunchableImpliesAttach(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if !c.Handoff.Launchable() {
			continue
		}
		if c.Attach == nil {
			t.Errorf("adapter %q: Launchable but no AttachSpec — every `observer <verb>` launcher attaches by default; add Attach: &AttachSpec{Subcommand: %q}", c.Tool, c.Handoff.Launch.Subcommand)
			continue
		}
		if c.Attach.Subcommand != c.Handoff.Launch.Subcommand {
			t.Errorf("adapter %q: Attach.Subcommand %q != Launch.Subcommand %q (must name the same wired launcher)",
				c.Tool, c.Attach.Subcommand, c.Handoff.Launch.Subcommand)
		}
	}
}

// TestNativeResumeGrounded pins the native-resume grounding rule: any row
// declaring Resume.Kind == ResumeNative must name a non-empty Subcommand
// AND IDMechanism — native resume is declared only for a tool whose
// resume argv has been verified live (mirroring how LaunchSpec was
// populated incrementally). It is vacuously green in Phase 0 (no
// ResumeNative row exists yet); that is intended — the pin arms the
// invariant before Phase 3 grounds the first native-resume tool.
func TestNativeResumeGrounded(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if c.Resume.Kind != integration.ResumeNative {
			continue
		}
		if c.Resume.Subcommand == "" {
			t.Errorf("adapter %q: ResumeNative but Subcommand empty", c.Tool)
		}
		if c.Resume.IDMechanism == "" {
			t.Errorf("adapter %q: ResumeNative but IDMechanism empty (name how the session id is passed)", c.Tool)
		}
	}
}

// TestReadersImplemented pins the shipped reader tranches: P1 (claude-code,
// codex) + the P2 tranche (cursor, cline, cline-cli, hermes, opencode).
func TestReadersImplemented(t *testing.T) {
	implemented := map[string]bool{}
	for _, a := range adapterdefaults.Adapters() {
		if _, ok := a.(transcriptReader); ok {
			implemented[a.Name()] = true
		}
	}
	for _, want := range []string{
		"claude-code", "codex", // P1
		"cursor", "cline", "cline-cli", "hermes", "opencode", // P2 tranche 2
	} {
		if !implemented[want] {
			t.Errorf("adapter %q must implement ReadTranscript (shipped tranche)", want)
		}
	}
}

// TestEveryLaunchableToolHasBinarySpec pins the binary-resolution coverage
// invariant: every adapter startable in the dashboard's embedded web
// terminal (Handoff.Launch != nil) MUST carry a grounded BinaryResolveSpec
// with at least one Unix binary name — the `observer <x>` launcher's
// resolution ladder (internal/toolresolve, Phase 2) dispatches on this row,
// so a launchable tool without one would resolve nothing. It mirrors
// TestLaunchableImpliesInjectPrompt: a launch capability without its
// resolution data is an incoherent row.
func TestEveryLaunchableToolHasBinarySpec(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if !c.Handoff.Launchable() {
			continue
		}
		if c.Binary == nil {
			t.Errorf("adapter %q: Launch set but Binary (BinaryResolveSpec) is nil", c.Tool)
			continue
		}
		if len(c.Binary.Names.Unix) == 0 {
			t.Errorf("adapter %q: Binary.Names.Unix is empty (a launchable tool needs at least one Unix binary name)", c.Tool)
		}
	}
}

// TestBinarySpecHonesty pins the honesty rules on every populated Binary
// row: install hints are complete (Argv + Display + Channel all present —
// never a fabricated/half-grounded command), probe dirs are HOME-RELATIVE
// and traversal-safe (non-empty, not absolute, no ".." segment), and every
// declared Windows spelling is non-empty. It walks every non-nil Binary,
// not just launchable rows, so a future non-launch resolution row is held
// to the same bar.
func TestBinarySpecHonesty(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if c.Binary == nil {
			continue
		}
		for i, h := range c.Binary.Installs {
			if len(h.Argv) == 0 {
				t.Errorf("adapter %q: Installs[%d] has empty Argv", c.Tool, i)
			}
			if h.Display == "" {
				t.Errorf("adapter %q: Installs[%d] has empty Display", c.Tool, i)
			}
			if h.Channel == "" {
				t.Errorf("adapter %q: Installs[%d] has empty Channel", c.Tool, i)
			}
		}
		for i, p := range c.Binary.ProbeDirs {
			if p.Rel == "" {
				t.Errorf("adapter %q: ProbeDirs[%d].Rel is empty", c.Tool, i)
			}
			if filepath.IsAbs(p.Rel) {
				t.Errorf("adapter %q: ProbeDirs[%d].Rel %q is absolute (must be HOME-relative)", c.Tool, i, p.Rel)
			}
			for _, seg := range strings.Split(p.Rel, "/") {
				if seg == ".." {
					t.Errorf("adapter %q: ProbeDirs[%d].Rel %q contains a '..' segment", c.Tool, i, p.Rel)
				}
			}
		}
		for i, w := range c.Binary.Names.Windows {
			if w == "" {
				t.Errorf("adapter %q: Names.Windows[%d] is empty", c.Tool, i)
			}
		}
	}
}

// authEnvClosureOwnedKeys are the launcher-closure-owned routing/profile env
// keys that AuthEnv must never carry — they are forwarded by the tool-specific
// attach closures (claudeAttachEnv / codexAttachEnv) or the proxy-route seam,
// not the credential-forwarding path. Mixing them into AuthEnv would double-
// forward (and, for the base URL, risk overriding the daemon's route).
var authEnvClosureOwnedKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":   true,
	"CLAUDE_CONFIG_DIR":    true,
	"ANTHROPIC_CONFIG_DIR": true,
	"CODEX_HOME":           true,
}

// TestAuthEnvWellFormed pins the honesty + hygiene rules on every AuthEnv row:
// each entry is a bare env-var NAME (never a value) — non-empty, no `=`, no
// whitespace, not OBSERVER_-prefixed, not one of the launcher-closure-owned
// routing/profile keys — and a row carries no intra-row duplicate. This is the
// guardrail behind the "NAMES only, never values" contract on Capability.AuthEnv.
func TestAuthEnvWellFormed(t *testing.T) {
	for _, c := range integration.Capabilities() {
		seen := map[string]bool{}
		for i, k := range c.AuthEnv {
			switch {
			case k == "":
				t.Errorf("adapter %q: AuthEnv[%d] is empty", c.Tool, i)
			case strings.ContainsAny(k, "="):
				t.Errorf("adapter %q: AuthEnv[%d] = %q contains '=' (a NAME, never a KEY=VALUE)", c.Tool, i, k)
			case strings.ContainsAny(k, " \t\n\r"):
				t.Errorf("adapter %q: AuthEnv[%d] = %q contains whitespace", c.Tool, i, k)
			case strings.HasPrefix(k, "OBSERVER_"):
				t.Errorf("adapter %q: AuthEnv[%d] = %q is OBSERVER_-prefixed (internal, never a credential env)", c.Tool, i, k)
			case authEnvClosureOwnedKeys[k]:
				t.Errorf("adapter %q: AuthEnv[%d] = %q is a launcher-closure-owned routing/profile key (forwarded elsewhere)", c.Tool, i, k)
			}
			if seen[k] {
				t.Errorf("adapter %q: AuthEnv has a duplicate %q", c.Tool, k)
			}
			seen[k] = true
		}
	}
}

// TestAuthEnvImpliesAttachable pins that credential-env is declared ONLY on an
// attachable row: forwarding is a property of the attach socket (the daemon-
// spawned child does not inherit the caller's os.Environ()), so a bare-only tool
// has nothing to forward across and must carry no AuthEnv.
func TestAuthEnvImpliesAttachable(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if len(c.AuthEnv) == 0 {
			continue
		}
		if c.Attach == nil {
			t.Errorf("adapter %q: AuthEnv set (%v) but the row is not attachable (Attach == nil)", c.Tool, c.AuthEnv)
		}
	}
}

// TestCrossOSRouteOnlyForPersistedKinds pins that ProxyRoute.CrossOSBridge
// is set only on the PERSISTED route kinds — RouteEnvSettings (claude-code
// → ~/.claude/settings.json) and RouteConfigFile (codex →
// ~/.codex/config.toml). The `<tool>-windows` virtual target writes the
// route into a foreign Windows home over crossmount; that only makes sense
// for a route backed by a config FILE observer writes, never a launcher
// env var (RouteLauncher) or an operator-pasted instruction (RouteManual).
func TestCrossOSRouteOnlyForPersistedKinds(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if c.Proxy == nil || !c.Proxy.CrossOSBridge {
			continue
		}
		switch c.Proxy.Kind {
		case integration.RouteEnvSettings, integration.RouteConfigFile:
			// persisted config write — cross-OS bridging is coherent.
		default:
			t.Errorf("adapter %q: Proxy.CrossOSBridge set on non-persisted RouteKind %q", c.Tool, c.Proxy.Kind)
		}
	}
}
