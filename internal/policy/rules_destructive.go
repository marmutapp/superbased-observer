package policy

import (
	"regexp"
	"strings"
)

// Destructive-action rules R-101…R-142 (spec §5.1) as ordered table
// rows, plus the shared command-analysis helpers they and the
// boundary rules build on. Every row's hit / safe-pattern / near-miss
// behavior is pinned in rules_destructive_test.go.

// destructiveRules returns the §5.1 table. Severity and the
// observe→enforce decision pair transcribe the catalog exactly.
func destructiveRules() []Rule {
	shell := []EventKind{KindShellExec}
	return []Rule{
		{
			ID: "R-101", Category: CategoryDestructive, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: matchDeleteCatastrophic,
			Observe: DecisionFlag, Enforce: DecisionDeny,
			SafePat: []SafeFn{safeUnitPathsAllowlisted},
			Doc:     "recursive delete targeting a filesystem root, the home directory, a root-depth wildcard, or a path outside the project",
			Advice:  "Delete inside the project root or a temp directory, or ask the operator to allow R-101 for this target.",
		},
		{
			ID: "R-102", Category: CategoryDestructive, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchDeleteVCS,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			SafePat: []SafeFn{safeUnitPathsAllowlisted},
			Doc:     "recursive delete inside the project targeting the VCS directory or the project root itself",
			Advice:  "Target a specific subdirectory; .git removal destroys history irrecoverably.",
		},
		{
			ID: "R-103", Category: CategoryDestructive, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchMassDelete,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			SafePat: []SafeFn{safeUnitPathsAllowlisted},
			Doc:     "mass-deletion chain (find -delete / xargs rm)",
			Advice:  "Prefer an explicit file list, or scope the find expression and re-run.",
		},
		{
			ID: "R-104", Category: CategoryDestructive, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchGitDiscard,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			SafePat: []SafeFn{safeGitDryRun},
			Doc:     "git command that discards uncommitted work (reset --hard / checkout -- / clean -f)",
			Advice:  "Stash first (git stash push -u), or run git clean -n to preview.",
		},
		{
			ID: "R-110", Category: CategoryDestructive, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: matchForcePush,
			Observe: DecisionFlag, Enforce: DecisionDeny,
			SafePat: []SafeFn{safeForceWithLease},
			Doc:     "force push to a protected branch",
			Advice:  "Use --force-with-lease, or ask the operator to allow R-110 for this project.",
		},
		{
			ID: "R-111", Category: CategoryDestructive, Severity: SeverityWarn,
			AppliesTo: shell, MatchCmd: matchProtectedDeletion,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "deletion of a protected branch or tag",
			Advice: "Confirm the ref is disposable; protected patterns are configured in [guard.boundary].",
		},
		{
			ID: "R-120", Category: CategoryDestructive, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: matchDangerousSQL,
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "destructive SQL through a database CLI (DROP/TRUNCATE/DELETE without WHERE)",
			Advice: "Add a WHERE clause or run against a scratch database; take a backup first.",
		},
		{
			ID: "R-130", Category: CategoryDestructive, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: matchCloudDestroy,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "cloud-infrastructure destruction command",
			Advice: "Run a plan/dry-run variant first and have a human review the target scope.",
		},
		{
			ID: "R-140", Category: CategoryDestructive, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchPackagePublish,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "package registry publish/yank",
			Advice: "Publishing is public and hard to retract; confirm version and registry.",
		},
		{
			ID: "R-141", Category: CategoryDestructive, Severity: SeverityHigh,
			AppliesTo: shell, MatchCmd: matchBulkPermChange,
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "bulk permission/ownership change (chmod -R 777 / chown -R outside project)",
			Advice: "Scope the change to specific files and use least-privilege modes.",
		},
		{
			ID: "R-142", Category: CategoryDestructive, Severity: SeverityCritical,
			AppliesTo: shell, MatchCmd: matchDiskDevice,
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "disk/device-level operation (mkfs / dd to a device / diskpart / format)",
			Advice: "Device operations are unrecoverable; this needs an operator, not an agent.",
		},
	}
}

// --- shared helpers -------------------------------------------------

// resolveOrLiteral resolves a path token like resolvePath, falling
// back to the home-expanded literal form when no cwd/project anchor
// exists (so "~/x" and "/abs" still analyze in anchor-less events).
func resolveOrLiteral(ctx *MatchContext, t string) string {
	ev := ctx.Event
	if rp := resolvePath(t, ctx.Cfg.Home, ev.Cwd, ev.ProjectRoot); rp != "" {
		return rp
	}
	return normPath(expandHome(t, ctx.Cfg.Home))
}

// hasWrapper reports whether the named wrapper was stripped from the
// unit.
func hasWrapper(cmd *Command, name string) bool {
	for _, w := range cmd.Wrappers {
		if w == name {
			return true
		}
	}
	return false
}

// unitPathTokens returns the unit's path-candidate tokens: pathish
// positionals plus all redirect targets (redirect targets are
// definitionally paths even when bare words).
func unitPathTokens(cmd *Command) []string {
	var out []string
	for _, p := range cmd.Positionals() {
		if looksPathish(p) {
			out = append(out, p)
		}
	}
	out = append(out, cmd.RedirectTargets...)
	return out
}

// excerpt bounds a detail string for verdict reasons (Don'ts: bounded
// excerpts only, never full content).
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// --- safe patterns ---------------------------------------------------

// matchAllowPaths reports whether a resolved path falls under
// [guard.boundary].allow_paths, matching both the absolute and the
// "~/"-anchored form (the config's default patterns are "~/"-anchored
// while resolved paths are absolute).
func matchAllowPaths(ctx *MatchContext, p string) bool {
	if p == "" {
		return false
	}
	if matchAnyGlob(ctx.Cfg.AllowPaths, p, true) {
		return true
	}
	if rel, ok := homeRelative(p, ctx.Cfg.Home); ok {
		return matchAnyGlob(ctx.Cfg.AllowPaths, rel, true)
	}
	return false
}

// safeUnitPathsAllowlisted exempts a unit whose path candidates ALL
// resolve under [guard.boundary].allow_paths (e.g. `rm -rf
// /tmp/build`). A unit with no path candidates is NOT safe — safety
// must be demonstrated, not absent.
func safeUnitPathsAllowlisted(ctx *MatchContext, cmd *Command) bool {
	if cmd == nil {
		return false
	}
	cand := unitPathTokens(cmd)
	if len(cand) == 0 {
		return false
	}
	for _, t := range cand {
		if !matchAllowPaths(ctx, resolveOrLiteral(ctx, t)) {
			return false
		}
	}
	return true
}

// safeGitDryRun exempts git invocations carrying a dry-run flag
// (`git clean -n` previews instead of deleting).
func safeGitDryRun(_ *MatchContext, cmd *Command) bool {
	return cmd != nil && cmd.Base == "git" &&
		(cmd.HasShortFlag('n') || cmd.HasLongFlag("--dry-run"))
}

// safeForceWithLease exempts the safe force-push variant (the
// catalog's canonical safe pattern for R-110).
func safeForceWithLease(_ *MatchContext, cmd *Command) bool {
	return cmd != nil && cmd.HasLongFlag("--force-with-lease", "--force-if-includes")
}

// --- delete-shape analysis (R-101/R-102) ------------------------------

// deleteShape recognizes filesystem-delete commands across dialects
// and reports whether the invocation is recursive. ok is false for
// non-delete bases.
func deleteShape(cmd *Command) (recursive, ok bool) {
	switch cmd.Base {
	case "rm":
		return cmd.HasShortFlag('r', 'R') || cmd.HasLongFlag("--recursive"), true
	case "remove-item":
		// PowerShell: -Recurse prefix-matched at ≥1 ("-r") — no
		// other Remove-Item parameter starts with r.
		return psHasParam(cmd, "recurse", 1), true
	case "rd", "rmdir":
		if cmd.Dialect == DialectCmd {
			return cmdHasFlag(cmd, "s"), true
		}
		return false, true // POSIX rmdir only removes empty dirs
	case "del", "erase":
		if cmd.Dialect == DialectCmd {
			return cmdHasFlag(cmd, "s"), true
		}
		return false, false
	default:
		return false, false
	}
}

// matchDeleteCatastrophic implements R-101: recursive delete whose
// target is a filesystem root, the home directory, a root-depth
// wildcard, or resolves outside the project root.
func matchDeleteCatastrophic(ctx *MatchContext, cmd *Command) (bool, string) {
	recursive, ok := deleteShape(cmd)
	if !ok || !recursive {
		return false, ""
	}
	for _, t := range cmd.Positionals() {
		if hit, why := dangerousDeleteTarget(ctx, t); hit {
			return true, why
		}
	}
	return false, ""
}

// dangerousDeleteTarget classifies one delete target. Resolution is
// lexical (pure package): home references expand, relatives anchor on
// Cwd/ProjectRoot, and an unresolvable relative is judged by its
// literal form only.
func dangerousDeleteTarget(ctx *MatchContext, t string) (bool, string) {
	raw := strings.TrimSpace(t)
	if raw == "" || strings.HasPrefix(raw, "-") {
		return false, ""
	}
	ev := ctx.Event
	home := ctx.Cfg.Home
	rp := resolvePath(raw, home, ev.Cwd, ev.ProjectRoot)
	cand := rp
	if cand == "" {
		cand = normPath(expandHome(raw, home))
	}
	switch {
	case isFilesystemRoot(cand):
		return true, "target " + raw + " is a filesystem root"
	case cand == "~" || (home != "" && pathsEqual(cand, home)):
		return true, "target " + raw + " is the home directory"
	case isRootDepthGlob(cand, home):
		return true, "target " + raw + " is a root-depth wildcard"
	case ev.ProjectRoot != "" && rp != "" && comparableFlavors(rp, ev.ProjectRoot) && !isUnder(rp, ev.ProjectRoot):
		return true, "target " + raw + " resolves outside the project root"
	default:
		return false, ""
	}
}

// matchDeleteVCS implements R-102: recursive delete inside the
// project that targets the .git directory or the project root
// itself. (The spec's ">N entries" half needs filesystem counting —
// I/O this pure package cannot do; deferred with a doc note, see
// PROGRESS.)
func matchDeleteVCS(ctx *MatchContext, cmd *Command) (bool, string) {
	recursive, ok := deleteShape(cmd)
	if !ok || !recursive {
		return false, ""
	}
	pr := ctx.Event.ProjectRoot
	if pr == "" {
		return false, ""
	}
	for _, t := range cmd.Positionals() {
		rp := resolveOrLiteral(ctx, t)
		if rp == "" || !isUnder(rp, pr) {
			continue
		}
		if hasComponent(rp, ".git") {
			return true, "target " + t + " includes the .git directory"
		}
		if pathsEqual(rp, pr) {
			return true, "target " + t + " is the project root itself"
		}
	}
	return false, ""
}

// matchMassDelete implements R-103: `find ... -delete`, `find -exec
// rm`, and xargs-fed rm chains.
func matchMassDelete(_ *MatchContext, cmd *Command) (bool, string) {
	if cmd.Base == "find" {
		if cmd.HasLongFlag("-delete") {
			return true, "find -delete mass deletion"
		}
		for i := 1; i < len(cmd.Argv)-1; i++ {
			a := cmd.Argv[i]
			if (a == "-exec" || a == "-execdir") && canonicalBase(cmd.Argv[i+1], cmd.Dialect) == "rm" {
				return true, "find -exec rm mass deletion"
			}
		}
		return false, ""
	}
	if cmd.Base == "rm" && hasWrapper(cmd, "xargs") {
		return true, "rm fed by xargs"
	}
	return false, ""
}

// --- git rules (R-104/R-110/R-111) ------------------------------------

// matchGitDiscard implements R-104: work-discarding git forms.
func matchGitDiscard(_ *MatchContext, cmd *Command) (bool, string) {
	if cmd.Base != "git" {
		return false, ""
	}
	switch cmd.Sub() {
	case "reset":
		if cmd.HasLongFlag("--hard") {
			return true, "git reset --hard discards uncommitted changes"
		}
	case "checkout":
		for _, a := range cmd.Argv[1:] {
			if a == "--" {
				return true, "git checkout -- discards working-tree changes"
			}
		}
	case "clean":
		if cmd.HasShortFlag('f') || cmd.HasLongFlag("--force") {
			return true, "git clean -f deletes untracked files"
		}
	}
	return false, ""
}

// refDst extracts the destination branch from a push refspec
// ("main", "+src:dst", "HEAD:main", "refs/heads/main").
func refDst(ref string) string {
	ref = strings.TrimPrefix(ref, "+")
	if i := strings.LastIndexByte(ref, ':'); i >= 0 {
		ref = ref[i+1:]
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

// protectedBranch reports whether name matches a configured protected
// pattern.
func protectedBranch(ctx *MatchContext, name string) bool {
	return name != "" && matchAnyGlob(ctx.Cfg.ProtectedBranches, name, false)
}

// matchForcePush implements R-110. With no refspec on the command
// line the current branch is unknown to a pure evaluator, so the rule
// hits conservatively (documented; the safe pattern and near-miss
// behavior are pinned by tests).
func matchForcePush(ctx *MatchContext, cmd *Command) (bool, string) {
	if cmd.Base != "git" || cmd.Sub() != "push" {
		return false, ""
	}
	if !cmd.HasShortFlag('f') && !cmd.HasLongFlag("--force") {
		return false, ""
	}
	pos := cmd.Positionals() // ["push", remote?, refspecs...]
	if len(pos) < 3 {
		return true, "force push with no refspec (target branch unknown, assumed protected)"
	}
	for _, ref := range pos[2:] {
		if b := refDst(ref); protectedBranch(ctx, b) {
			return true, "force push to protected branch " + b
		}
	}
	return false, ""
}

// matchProtectedDeletion implements R-111: branch -D / tag -d /
// push --delete (and ":ref" push refspecs) against protected
// patterns.
func matchProtectedDeletion(ctx *MatchContext, cmd *Command) (bool, string) {
	if cmd.Base != "git" {
		return false, ""
	}
	pos := cmd.Positionals()
	switch cmd.Sub() {
	case "branch":
		if cmd.HasShortFlag('D') || (cmd.HasLongFlag("--delete") && cmd.HasLongFlag("--force")) {
			for _, b := range pos[1:] {
				if protectedBranch(ctx, b) {
					return true, "deleting protected branch " + b
				}
			}
		}
	case "tag":
		if cmd.HasShortFlag('d') || cmd.HasLongFlag("--delete") {
			for _, t := range pos[1:] {
				if protectedBranch(ctx, t) {
					return true, "deleting protected tag " + t
				}
			}
		}
	case "push":
		for _, ref := range pos[1:] {
			deleting := cmd.HasLongFlag("--delete") || strings.HasPrefix(ref, ":")
			if !deleting {
				continue
			}
			if b := refDst(ref); protectedBranch(ctx, b) {
				return true, "push-deleting protected ref " + b
			}
		}
	}
	return false, ""
}

// --- SQL (R-120) -------------------------------------------------------

// sqlCLIs are the database CLIs whose command-line SQL and heredoc
// bodies R-120 scans.
var sqlCLIs = map[string]bool{"psql": true, "mysql": true, "mariadb": true, "sqlite3": true}

// SQL shape patterns. Go regexp is RE2 — linear time, no
// catastrophic backtracking (a documented advantage over PCRE-based
// competitors, spec §4.4).
var (
	reSQLDrop     = regexp.MustCompile(`(?i)\bDROP\s+(TABLE|DATABASE|SCHEMA)\b`)
	reSQLTruncate = regexp.MustCompile(`(?i)\bTRUNCATE(\s+TABLE)?\s+\w`)
	reSQLDelete   = regexp.MustCompile(`(?i)\bDELETE\s+FROM\b`)
	reSQLWhere    = regexp.MustCompile(`(?i)\bWHERE\b`)
)

// matchDangerousSQL implements R-120 over -c/-e command flags, the
// sqlite3 positional SQL argument, and heredoc bodies.
func matchDangerousSQL(_ *MatchContext, cmd *Command) (bool, string) {
	if !sqlCLIs[cmd.Base] {
		return false, ""
	}
	var texts []string
	if v, ok := cmd.FlagValue("-c", "--command", "-e", "--execute"); ok {
		texts = append(texts, v)
	}
	if cmd.Base == "sqlite3" {
		if pos := cmd.Positionals(); len(pos) > 1 {
			texts = append(texts, pos[len(pos)-1])
		}
	}
	if cmd.Heredoc != "" {
		texts = append(texts, cmd.Heredoc)
	}
	for _, t := range texts {
		if hit, why := dangerousSQL(t); hit {
			return true, why
		}
	}
	return false, ""
}

// dangerousSQL scans SQL text statement-by-statement for the R-120
// shapes.
func dangerousSQL(text string) (bool, string) {
	for _, stmt := range strings.Split(text, ";") {
		switch {
		case reSQLDrop.MatchString(stmt):
			return true, "DROP statement: " + excerpt(stmt)
		case reSQLTruncate.MatchString(stmt):
			return true, "TRUNCATE statement: " + excerpt(stmt)
		case reSQLDelete.MatchString(stmt) && !reSQLWhere.MatchString(stmt):
			return true, "DELETE without WHERE: " + excerpt(stmt)
		}
	}
	return false, ""
}

// --- cloud / publish / perms / device (R-130/R-140/R-141/R-142) -------

// cloudDestroyChecks is the R-130 sub-table: one check per provider
// CLI shape (table-driven per Module rule 5; each entry pinned by its
// own test case).
var cloudDestroyChecks = []func(*Command) (bool, string){
	checkTerraformDestroy,
	checkAWSDestroy,
	checkGcloudDelete,
	checkKubectlDelete,
	checkHelmUninstall,
}

// matchCloudDestroy implements R-130 by walking the provider
// sub-table.
func matchCloudDestroy(_ *MatchContext, cmd *Command) (bool, string) {
	for _, check := range cloudDestroyChecks {
		if hit, why := check(cmd); hit {
			return true, why
		}
	}
	return false, ""
}

// checkTerraformDestroy: `terraform destroy` (any) and `terraform
// apply -auto-approve` (unattended apply).
func checkTerraformDestroy(cmd *Command) (bool, string) {
	if cmd.Base != "terraform" && cmd.Base != "tofu" {
		return false, ""
	}
	switch cmd.Sub() {
	case "destroy":
		return true, "terraform destroy"
	case "apply":
		if cmd.HasLongFlag("-auto-approve", "--auto-approve") {
			return true, "terraform apply -auto-approve (unattended)"
		}
	}
	return false, ""
}

// checkAWSDestroy: `aws s3 rm --recursive` and `aws ec2
// terminate-instances`.
func checkAWSDestroy(cmd *Command) (bool, string) {
	if cmd.Base != "aws" {
		return false, ""
	}
	pos := cmd.Positionals()
	if len(pos) >= 2 && pos[0] == "s3" && pos[1] == "rm" && cmd.HasLongFlag("--recursive") {
		return true, "aws s3 rm --recursive"
	}
	if len(pos) >= 2 && pos[0] == "ec2" && pos[1] == "terminate-instances" {
		return true, "aws ec2 terminate-instances"
	}
	return false, ""
}

// checkGcloudDelete: any gcloud command with a delete verb.
func checkGcloudDelete(cmd *Command) (bool, string) {
	if cmd.Base != "gcloud" {
		return false, ""
	}
	for _, p := range cmd.Positionals() {
		if p == "delete" {
			return true, "gcloud ... delete"
		}
	}
	return false, ""
}

// checkKubectlDelete: kubectl delete with --all/--all-namespaces or a
// namespace-wide target.
func checkKubectlDelete(cmd *Command) (bool, string) {
	if cmd.Base != "kubectl" || cmd.Sub() != "delete" {
		return false, ""
	}
	if cmd.HasLongFlag("--all", "--all-namespaces") || cmd.HasShortFlag('A') {
		return true, "kubectl delete --all"
	}
	pos := cmd.Positionals()
	if len(pos) >= 2 && (pos[1] == "namespace" || pos[1] == "ns") {
		return true, "kubectl delete namespace"
	}
	return false, ""
}

// checkHelmUninstall: helm uninstall (and its legacy delete alias).
func checkHelmUninstall(cmd *Command) (bool, string) {
	if cmd.Base != "helm" {
		return false, ""
	}
	if s := cmd.Sub(); s == "uninstall" || s == "delete" {
		return true, "helm " + s
	}
	return false, ""
}

// matchPackagePublish implements R-140: registry publish and yank
// operations across the package managers in the catalog.
func matchPackagePublish(_ *MatchContext, cmd *Command) (bool, string) {
	sub := cmd.Sub()
	switch cmd.Base {
	case "npm", "yarn", "pnpm":
		if sub == "publish" || sub == "unpublish" {
			return true, cmd.Base + " " + sub
		}
	case "twine":
		if sub == "upload" {
			return true, "twine upload (PyPI publish)"
		}
	case "cargo":
		if sub == "publish" || sub == "yank" {
			return true, "cargo " + sub
		}
	case "gem":
		if sub == "push" || sub == "yank" {
			return true, "gem " + sub
		}
	}
	return false, ""
}

// reMode777 recognizes the world-writable numeric modes.
var reMode777 = regexp.MustCompile(`^0?[0-7]?777$`)

// matchBulkPermChange implements R-141: chmod -R 777 anywhere, and
// chown -R with a target outside the project root.
func matchBulkPermChange(ctx *MatchContext, cmd *Command) (bool, string) {
	recursive := cmd.HasShortFlag('R') || cmd.HasLongFlag("--recursive")
	if !recursive {
		return false, ""
	}
	pos := cmd.Positionals()
	switch cmd.Base {
	case "chmod":
		for _, p := range pos {
			if reMode777.MatchString(p) || p == "a+rwx" || p == "a=rwx" {
				return true, "chmod -R " + p
			}
		}
	case "chown":
		if len(pos) < 2 {
			return false, ""
		}
		for _, t := range pos[1:] { // pos[0] is the owner spec
			rp := resolveOrLiteral(ctx, t)
			if rp == "" || ctx.Event.ProjectRoot == "" {
				continue
			}
			if comparableFlavors(rp, ctx.Event.ProjectRoot) && !isUnder(rp, ctx.Event.ProjectRoot) {
				return true, "chown -R outside project: " + t
			}
		}
	}
	return false, ""
}

// matchDiskDevice implements R-142: mkfs*, dd writing to a device,
// diskpart, cmd-dialect format, and the PowerShell volume cmdlets.
// A POSIX-dialect bare `format` is deliberately NOT matched (too
// collision-prone with project scripts); the cmd dialect and
// Format-Volume cover the real Windows shapes.
func matchDiskDevice(_ *MatchContext, cmd *Command) (bool, string) {
	switch {
	case strings.HasPrefix(cmd.Base, "mkfs"):
		return true, cmd.Base + " filesystem creation"
	case cmd.Base == "diskpart":
		return true, "diskpart disk partitioning"
	case cmd.Base == "format" && cmd.Dialect == DialectCmd:
		return true, "format volume"
	case cmd.Base == "format-volume" || cmd.Base == "clear-disk":
		return true, cmd.Base + " volume operation"
	case cmd.Base == "dd":
		for _, a := range cmd.Argv[1:] {
			la := strings.ToLower(a)
			if strings.HasPrefix(la, "of=/dev/") || strings.HasPrefix(la, `of=\\.\`) {
				return true, "dd writing to device " + a
			}
		}
	}
	return false, ""
}
