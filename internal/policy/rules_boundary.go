package policy

import "strings"

// Boundary & sensitive-path rules R-150…R-161 (spec §5.2). Path rules
// generally come in row pairs/quads per public ID (approved deviation
// 3): file-access rows match the resolved Event path; shell rows scan
// command units for path-shaped tokens, classified read vs write.
// Every row is pinned in rules_boundary_test.go, including the
// Windows shapes (%APPDATA%, PowerShell profiles, Run keys) — a
// Windows-first-arc requirement.

// actionIsWrite reports whether the models-taxonomy action verb
// mutates the target. The literals mirror internal/models (not
// imported — purity); unknown/empty verbs classify as reads, the
// less-alarming direction.
func actionIsWrite(actionType string) bool {
	switch actionType {
	case "write_file", "edit_file":
		return true
	default:
		return false
	}
}

// isWriteEvent reports whether the event as a whole is a write:
// write-verb file accesses, and config-change events by definition.
func isWriteEvent(ev *Event) bool {
	return ev.Kind == KindConfigChange || actionIsWrite(ev.ActionType)
}

// pathPattern is one row of a path-rule table: a glob (matched
// case-insensitively — conservative: correct on case-insensitive
// filesystems, negligible-FP on case-sensitive ones since legit
// different-case twins of ~/.ssh etc. don't occur) plus the
// human-readable description used in verdict reasons.
type pathPattern struct {
	glob string
	desc string
}

// sensitivePathPatterns is the R-152 sensitive set (spec §5.2):
// credential stores, browser profiles, OS keychains, wallet dirs —
// in "~/"-anchored form plus absolute Windows fallbacks for
// cross-flavor events whose home is unknown.
var sensitivePathPatterns = []pathPattern{
	{"~/.ssh/**", "SSH key material"},
	{"~/.aws/**", "AWS credentials"},
	{"~/.gnupg/**", "GnuPG keyring"},
	{"~/.kube/config", "Kubernetes credentials"},
	{"~/.netrc", "netrc credentials"},
	{"~/_netrc", "netrc credentials"},
	{"~/.npmrc", "npm auth configuration"},
	{"~/.docker/config.json", "Docker registry auth"},
	{"~/.config/gcloud/**", "gcloud credentials"},
	{"~/.azure/**", "Azure credentials"},
	{"~/.mozilla/firefox/**", "browser profile"},
	{"~/.config/google-chrome/**", "browser profile"},
	{"~/.config/chromium/**", "browser profile"},
	{"~/library/application support/google/chrome/**", "browser profile"},
	{"~/appdata/roaming/mozilla/firefox/**", "browser profile"},
	{"~/appdata/local/google/chrome/user data/**", "browser profile"},
	{"?:/users/*/.ssh/**", "SSH key material"},
	{"?:/users/*/.aws/**", "AWS credentials"},
	{"?:/users/*/appdata/roaming/mozilla/firefox/**", "browser profile"},
	{"?:/users/*/appdata/local/google/chrome/user data/**", "browser profile"},
	{"~/library/keychains/**", "macOS keychain"},
	{"~/.local/share/keyrings/**", "GNOME keyring"},
	{"~/appdata/roaming/microsoft/protect/**", "Windows DPAPI store"},
	{"?:/users/*/appdata/roaming/microsoft/protect/**", "Windows DPAPI store"},
	{"~/.bitcoin/**", "cryptocurrency wallet"},
	{"~/.electrum/**", "cryptocurrency wallet"},
	{"~/.ethereum/**", "cryptocurrency wallet"},
}

// secretFileBasenames is the R-153 secret-file set, matched against
// the target's basename.
var secretFileBasenames = []string{
	".env", ".env.*", "*.pem", "*.key",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "credentials*.json",
}

// secretExampleBasenames are the placeholder variants R-153 must NOT
// fire on (.env.example committed to repos — the classic FP).
var secretExampleBasenames = []string{"*.example", "*.sample", "*.template", "*.dist"}

// shellProfilePatterns is the R-154 persistence surface: shell rc /
// profile files across POSIX shells and both PowerShell generations
// (including OneDrive-redirected Documents).
var shellProfilePatterns = []pathPattern{
	{"~/.bashrc", "bash rc file"},
	{"~/.bash_profile", "bash profile"},
	{"~/.bash_login", "bash login file"},
	{"~/.profile", "shell profile"},
	{"~/.zshrc", "zsh rc file"},
	{"~/.zprofile", "zsh profile"},
	{"~/.zshenv", "zsh env file"},
	{"~/.zlogin", "zsh login file"},
	{"~/.config/fish/config.fish", "fish config"},
	{"~/documents/powershell/*profile*.ps1", "PowerShell profile"},
	{"~/documents/windowspowershell/*profile*.ps1", "PowerShell profile"},
	{"~/onedrive/documents/powershell/*profile*.ps1", "PowerShell profile"},
	{"~/onedrive/documents/windowspowershell/*profile*.ps1", "PowerShell profile"},
	{"?:/users/*/documents/powershell/*profile*.ps1", "PowerShell profile"},
	{"?:/users/*/documents/windowspowershell/*profile*.ps1", "PowerShell profile"},
}

// persistencePathPatterns is the R-155 path surface: cron, systemd
// user units, LaunchAgents/Daemons, XDG autostart, Windows Startup
// folders.
var persistencePathPatterns = []pathPattern{
	{"/etc/crontab", "system crontab"},
	{"/etc/cron*/**", "system cron directory"},
	{"/var/spool/cron/**", "user crontab spool"},
	{"~/.config/systemd/user/**", "systemd user unit"},
	{"/etc/systemd/**", "systemd unit"},
	{"~/library/launchagents/**", "LaunchAgent"},
	{"/library/launchagents/**", "system LaunchAgent"},
	{"/library/launchdaemons/**", "LaunchDaemon"},
	{"~/.config/autostart/**", "XDG autostart entry"},
	{"~/appdata/roaming/microsoft/windows/start menu/programs/startup/**", "Windows Startup folder"},
	{"?:/users/*/appdata/roaming/microsoft/windows/start menu/programs/startup/**", "Windows Startup folder"},
	{"?:/programdata/microsoft/windows/start menu/programs/startup/**", "Windows Startup folder"},
}

// gitHooksPatterns is the R-156 surface: in-repo git hooks, a
// persistence vector that executes on the next git operation.
var gitHooksPatterns = []pathPattern{
	{"**/.git/hooks/**", "git hook"},
}

// observerConfigPatterns is the R-160 surface: observer's own
// configuration plus the client hook-bearing config files an agent
// could edit to unhook us (gap F4 — tamper-EVIDENT, with self-heal at
// observer start). Project-level .claude/settings.local.json is
// deliberately ABSENT: the host tool itself writes it on every
// permission grant; flagging it would be constant noise and denying
// it would break the host tool (revisit with the G6 conformance
// matrix).
var observerConfigPatterns = []pathPattern{
	{"~/.observer/**", "observer configuration"},
	{"?:/users/*/.observer/**", "observer configuration"},
	{"~/.claude/settings.json", "Claude Code hook settings"},
	{"**/.claude/settings.json", "project Claude Code hook settings"},
	{"~/.codex/config.toml", "Codex configuration"},
}

// clientConfigDirPatterns is the R-150 exclusion set: the AI clients'
// own config/state dirs, which their tools touch routinely and
// legitimately from any project (spec §5.2 "the client's own config
// dir").
var clientConfigDirPatterns = []string{
	"~/.claude/**", "~/.codex/**", "~/.cursor/**", "~/.observer/**",
	"~/.gemini/**", "~/.config/opencode/**", "~/.local/share/opencode/**",
	"~/.cline/**", "~/.copilot/**",
}

// boundaryRules returns the §5.2 table. Path rules use the shared
// faPathMatch/shPathMatch constructors so matching and write/read
// classification stay in ONE place.
func boundaryRules() []Rule {
	fa := []EventKind{KindFileAccess}
	faCfg := []EventKind{KindFileAccess, KindConfigChange}
	shell := []EventKind{KindShellExec}
	boundarySafe := []SafeFn{safePathAllowlisted, safeClientConfigPath}
	return []Rule{
		{
			ID: "R-150", Category: CategoryBoundary, Severity: SeverityWarn,
			AppliesTo: fa, Match: matchOutsideProject(false),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			SafePat: boundarySafe,
			Doc:     "file read outside the project root",
			Advice:  "Reads outside the project are recorded; add a [guard.boundary] allow_paths entry if this location is routine.",
		},
		{
			ID: "R-150", Category: CategoryBoundary, Severity: SeverityWarn,
			AppliesTo: fa, Match: matchOutsideProject(true),
			Observe: DecisionFlag, Enforce: DecisionAsk,
			SafePat: boundarySafe,
			Doc:     "file write outside the project root",
			Advice:  "Write inside the project, or add a [guard.boundary] allow_paths entry for this location.",
		},
		{
			ID: "R-151", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: fa, Match: matchCrossProjectWrite,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "write into a DIFFERENT observed project's root (cross-project bleed)",
			Advice: "Open that project in its own session instead of writing across project boundaries.",
		},
		{
			ID: "R-152", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: fa, Match: faPathMatch(sensitivePathPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "write to a sensitive credential/profile location",
			Advice: "Credential stores are off-limits to agents; have the operator make this change.",
		},
		{
			ID: "R-152", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: fa, Match: faPathMatch(sensitivePathPatterns, false),
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "read of a sensitive credential/profile location",
			Advice: "If a credential is genuinely needed, ask the operator to provide it explicitly.",
		},
		{
			ID: "R-152", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: shPathMatch(sensitivePathPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "shell command writing to a sensitive credential/profile location",
			Advice: "Credential stores are off-limits to agents; have the operator make this change.",
		},
		{
			ID: "R-152", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: shPathMatch(sensitivePathPatterns, false),
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "shell command reading a sensitive credential/profile location",
			Advice: "If a credential is genuinely needed, ask the operator to provide it explicitly.",
		},
		{
			ID: "R-153", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: fa, Match: matchSecretFileReadFA,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			SafePat: []SafeFn{safeSecretExampleFile},
			Doc:     "read of a secret-bearing file (.env / key material / credentials)",
			Advice:  "Use a placeholder (.env.example) or have the operator supply the value.",
		},
		{
			ID: "R-153", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchSecretFileReadShell,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "shell command reading a secret-bearing file (.env / key material / credentials)",
			Advice: "Use a placeholder (.env.example) or have the operator supply the value.",
		},
		{
			ID: "R-154", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: faCfg, Match: faPathMatch(shellProfilePatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "write to a shell rc/profile file (persistence vector)",
			Advice: "Profile changes outlive the session; have the operator apply them.",
		},
		{
			ID: "R-154", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: shPathMatch(shellProfilePatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "shell command writing to a shell rc/profile file (persistence vector)",
			Advice: "Profile changes outlive the session; have the operator apply them.",
		},
		{
			ID: "R-155", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: faCfg, Match: faPathMatch(persistencePathPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "write to an autostart/persistence location (cron, systemd, LaunchAgents, Startup)",
			Advice: "Persistent scheduling needs the operator; agents must not install autostart entries.",
		},
		{
			ID: "R-155", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: shPathMatch(persistencePathPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "shell command writing to an autostart/persistence location",
			Advice: "Persistent scheduling needs the operator; agents must not install autostart entries.",
		},
		{
			ID: "R-155", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: matchPersistenceCommand,
			Observe: DecisionFlag, Enforce: DecisionDeny,
			SafePat: []SafeFn{safeCrontabList},
			Doc:     "persistence-installing command (crontab / schtasks /create / Run registry key / systemctl enable)",
			Advice:  "Persistent scheduling needs the operator; agents must not install autostart entries.",
		},
		{
			ID: "R-156", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: faCfg, Match: faPathMatch(gitHooksPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "write to .git/hooks (in-repo persistence vector)",
			Advice: "Git hooks execute on the next git operation; review the hook body before allowing.",
		},
		{
			ID: "R-156", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: shPathMatch(gitHooksPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "shell command writing to .git/hooks (in-repo persistence vector)",
			Advice: "Git hooks execute on the next git operation; review the hook body before allowing.",
		},
		{
			ID: "R-160", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: faCfg, Match: faPathMatch(observerConfigPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "agent modifying observer/guard/hook configuration",
			Advice: "Guard and hook configuration is operator-owned (F4); ask the operator to change it.",
		},
		{
			ID: "R-160", Category: CategoryBoundary, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: shPathMatch(observerConfigPatterns, true),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "shell command modifying observer/guard/hook configuration",
			Advice: "Guard and hook configuration is operator-owned (F4); ask the operator to change it.",
		},
		{
			ID: "R-161", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: faCfg, Match: matchProjectPolicyWriteFA,
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "agent modifying the project guard policy file",
			Advice: "Project policy edits are recorded; §4.6 layering prevents loosening regardless.",
		},
		{
			ID: "R-161", Category: CategoryBoundary, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchProjectPolicyWriteShell,
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "shell command modifying the project guard policy file",
			Advice: "Project policy edits are recorded; §4.6 layering prevents loosening regardless.",
		},
	}
}

// --- shared path matching ---------------------------------------------

// homeRelative rewrites an absolute path under home into its
// "~/"-anchored form for pattern matching. ok is false when p is not
// under home (or home is unknown).
func homeRelative(p, home string) (string, bool) {
	np, nh := normPath(p), normPath(home)
	if nh == "" || !isUnder(np, nh) {
		return "", false
	}
	if len(np) <= len(nh) {
		return "~", true
	}
	return "~/" + np[len(nh)+1:], true
}

// pathPatternMatch matches a resolved path against a pattern table,
// trying both the absolute form and (when under home) the
// "~/"-anchored form. Unexpanded literal "~/..." paths (empty-home
// degradation) match the "~" patterns directly.
func pathPatternMatch(p, home string, pats []pathPattern) (string, bool) {
	if p == "" {
		return "", false
	}
	forms := []string{p}
	if rel, ok := homeRelative(p, home); ok {
		forms = append(forms, rel)
	}
	for _, pat := range pats {
		for _, f := range forms {
			if matchGlob(pat.glob, f, true) {
				return pat.desc, true
			}
		}
	}
	return "", false
}

// faPathMatch builds a file-access matcher over a pattern table for
// the given write/read direction.
func faPathMatch(pats []pathPattern, wantWrite bool) MatchFn {
	return func(ctx *MatchContext) (bool, string) {
		if isWriteEvent(ctx.Event) != wantWrite {
			return false, ""
		}
		desc, ok := pathPatternMatch(ctx.Path, ctx.Cfg.Home, pats)
		if !ok {
			return false, ""
		}
		return true, desc + " (" + ctx.Event.Target + ")"
	}
}

// shPathMatch builds a command-unit matcher over a pattern table:
// every path-candidate token resolves and matches against the table,
// then classifies as write or read access within the unit.
func shPathMatch(pats []pathPattern, wantWrite bool) MatchCmdFn {
	return func(ctx *MatchContext, cmd *Command) (bool, string) {
		for _, t := range unitPathTokens(cmd) {
			rp := resolveOrLiteral(ctx, t)
			desc, ok := pathPatternMatch(rp, ctx.Cfg.Home, pats)
			if !ok {
				continue
			}
			if classifyWrite(cmd, t) == wantWrite {
				return true, desc + " (" + t + ")"
			}
		}
		return false, ""
	}
}

// writeBases are command bases whose pathish operands are written
// regardless of position.
var writeBases = map[string]bool{
	"tee": true, "truncate": true, "install": true,
	"set-content": true, "add-content": true, "out-file": true, "new-item": true,
}

// destBases are copy/move-style commands whose LAST pathish operand
// is the written destination.
var destBases = map[string]bool{
	"cp": true, "mv": true, "rsync": true, "scp": true,
	"copy-item": true, "move-item": true,
}

// classifyWrite classifies one path token's access direction within a
// unit (documented approximation, pinned by tests): redirect targets
// are writes; writeBases write all operands; copy/move write their
// last operand; sed -i writes in place; delete commands write
// (destroy) their targets; everything else reads.
func classifyWrite(cmd *Command, tok string) bool {
	for _, r := range cmd.RedirectTargets {
		if r == tok {
			return true
		}
	}
	if writeBases[cmd.Base] {
		return true
	}
	if _, isDelete := deleteShape(cmd); isDelete {
		return true
	}
	if cmd.Base == "sed" && (cmd.HasShortFlag('i') || cmd.HasLongFlag("--in-place")) {
		return true
	}
	if destBases[cmd.Base] {
		pathish := unitPathTokens(cmd)
		return len(pathish) >= 2 && pathish[len(pathish)-1] == tok
	}
	return false
}

// --- R-150 / R-151 ----------------------------------------------------

// matchOutsideProject builds the R-150 matcher for the given
// direction. No project root (or a cross-flavor path the boundary
// didn't translate) means no hit — unknown is not a violation.
func matchOutsideProject(wantWrite bool) MatchFn {
	return func(ctx *MatchContext) (bool, string) {
		if isWriteEvent(ctx.Event) != wantWrite {
			return false, ""
		}
		p, pr := ctx.Path, ctx.Event.ProjectRoot
		if p == "" || pr == "" || !comparableFlavors(p, pr) || isUnder(p, pr) {
			return false, ""
		}
		return true, "path " + ctx.Event.Target + " is outside the project root"
	}
}

// matchCrossProjectWrite implements R-151: a write landing inside a
// DIFFERENT observed project's root — detectable only because the
// daemon knows every project root it watches.
func matchCrossProjectWrite(ctx *MatchContext) (bool, string) {
	if !isWriteEvent(ctx.Event) || ctx.Path == "" {
		return false, ""
	}
	cur := ctx.Event.ProjectRoot
	if cur != "" && isUnder(ctx.Path, cur) {
		return false, "" // inside the current project: never cross-bleed
	}
	for _, root := range ctx.Cfg.KnownProjectRoots {
		if root == "" || (cur != "" && pathsEqual(root, cur)) {
			continue
		}
		if isUnder(ctx.Path, root) {
			return true, "write into observed project " + root
		}
	}
	return false, ""
}

// safePathAllowlisted exempts file-access paths under
// [guard.boundary].allow_paths (both absolute and "~/"-anchored
// forms, via matchAllowPaths).
func safePathAllowlisted(ctx *MatchContext, _ *Command) bool {
	return matchAllowPaths(ctx, ctx.Path)
}

// safeClientConfigPath exempts the AI clients' own config/state dirs
// (touched routinely from any project — R-150's catalog exclusion).
func safeClientConfigPath(ctx *MatchContext, _ *Command) bool {
	if ctx.Path == "" {
		return false
	}
	rel, ok := homeRelative(ctx.Path, ctx.Cfg.Home)
	if !ok {
		rel = ctx.Path // unexpanded "~/..." literals match directly
	}
	return matchAnyGlob(clientConfigDirPatterns, rel, true)
}

// --- R-153 -------------------------------------------------------------

// secretBasename reports whether name matches the R-153 secret-file
// set.
func secretBasename(name string) bool {
	if name == "" {
		return false
	}
	return matchAnyGlob(secretFileBasenames, name, true)
}

// exampleBasename reports whether name is a placeholder variant
// (.env.example etc.).
func exampleBasename(name string) bool {
	return matchAnyGlob(secretExampleBasenames, name, true)
}

// matchSecretFileReadFA implements the R-153 file-access row.
func matchSecretFileReadFA(ctx *MatchContext) (bool, string) {
	if isWriteEvent(ctx.Event) {
		return false, ""
	}
	if !secretBasename(baseName(ctx.Path)) {
		return false, ""
	}
	return true, "secret file " + ctx.Event.Target
}

// matchSecretFileReadShell implements the R-153 shell row. Unlike the
// generic path rules it scans ALL positionals, not just pathish ones:
// the canonical `cat .env` carries a bare dotfile token, and the
// secret-basename glob itself is the precision filter. The
// example-file exclusion is token-level here (a unit can touch both a
// real and a placeholder file), unlike the file-access row where it
// is a rule-level safe pattern.
func matchSecretFileReadShell(ctx *MatchContext, cmd *Command) (bool, string) {
	for _, t := range append(cmd.Positionals(), cmd.RedirectTargets...) {
		name := baseName(resolveOrLiteral(ctx, t))
		if !secretBasename(name) || exampleBasename(name) {
			continue
		}
		if !classifyWrite(cmd, t) {
			return true, "secret file " + t
		}
	}
	return false, ""
}

// safeSecretExampleFile exempts placeholder files on the R-153
// file-access row.
func safeSecretExampleFile(ctx *MatchContext, _ *Command) bool {
	return exampleBasename(baseName(ctx.Path))
}

// --- R-155 command shapes ----------------------------------------------

// runKeyToken reports whether a token references the Windows
// CurrentVersion\Run registry persistence keys.
func runKeyToken(t string) bool {
	low := strings.ToLower(strings.ReplaceAll(t, "/", `\`))
	return strings.Contains(low, `\currentversion\run`)
}

// matchPersistenceCommand implements the R-155 command row:
// crontab installs, schtasks /create, Run-key registry writes, and
// systemctl enable. (launchctl load is deliberately absent — routine
// for dev services; the LaunchAgents PATH row catches actual
// persistence writes.)
func matchPersistenceCommand(_ *MatchContext, cmd *Command) (bool, string) {
	switch cmd.Base {
	case "crontab":
		return true, "crontab modification"
	case "schtasks":
		if cmdHasFlag(cmd, "create") {
			return true, "schtasks /create scheduled task"
		}
	case "reg":
		if cmd.Sub() == "add" {
			for _, a := range cmd.Argv[1:] {
				if runKeyToken(a) {
					return true, "Run registry key write"
				}
			}
		}
	case "set-itemproperty", "new-itemproperty":
		for _, a := range cmd.Argv[1:] {
			if runKeyToken(a) {
				return true, "Run registry key write"
			}
		}
	case "systemctl":
		for _, p := range cmd.Positionals() {
			if p == "enable" {
				return true, "systemctl enable (persistent unit)"
			}
		}
	}
	return false, ""
}

// safeCrontabList exempts the read-only `crontab -l` listing.
func safeCrontabList(_ *MatchContext, cmd *Command) bool {
	return cmd != nil && cmd.Base == "crontab" && cmd.HasShortFlag('l')
}

// --- R-161 -------------------------------------------------------------

// projectPolicyPath reports whether p is a PROJECT guard policy file
// (<project>/.observer/guard-policy.toml) as opposed to the
// user-level ~/.observer one, which is R-160's territory.
func projectPolicyPath(p, home string) bool {
	if p == "" || !matchGlob("**/.observer/guard-policy.toml", p, true) {
		return false
	}
	if home != "" && isUnder(p, normPath(home)+"/.observer") {
		return false
	}
	return true
}

// matchProjectPolicyWriteFA implements the R-161 file-access row.
func matchProjectPolicyWriteFA(ctx *MatchContext) (bool, string) {
	if !isWriteEvent(ctx.Event) || !projectPolicyPath(ctx.Path, ctx.Cfg.Home) {
		return false, ""
	}
	return true, "project guard policy " + ctx.Event.Target
}

// matchProjectPolicyWriteShell implements the R-161 shell row.
func matchProjectPolicyWriteShell(ctx *MatchContext, cmd *Command) (bool, string) {
	for _, t := range unitPathTokens(cmd) {
		rp := resolveOrLiteral(ctx, t)
		if projectPolicyPath(rp, ctx.Cfg.Home) && classifyWrite(cmd, t) {
			return true, "project guard policy " + t
		}
	}
	return false, ""
}
