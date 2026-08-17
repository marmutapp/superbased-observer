package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// bwrap flag vocabulary (0.4.0 floor). The literals live here so the exact
// spelling has one owner and no caller re-types them.
const (
	flagROBind        = "--ro-bind"
	flagROBindTry     = "--ro-bind-try"
	flagBind          = "--bind"
	flagBindTry       = "--bind-try"
	flagTmpfs         = "--tmpfs"
	flagProc          = "--proc"
	flagDev           = "--dev"
	flagChdir         = "--chdir"
	flagDieWithParent = "--die-with-parent"
	// argSep is bwrap's end-of-options marker; the inner argv follows it.
	argSep = "--"

	// homeModeReadonly omits the $HOME tmpfs (the whole-root ro-bind still
	// makes home read-only); anything else means tmpfs-blind the home.
	homeModeReadonly = "readonly"
	homeModeTmpfs    = "tmpfs"

	// workspacesLeaf is the managed-workspaces subdir under ~/.observer that is
	// tmpfs-blinded before this run's workspace is punched back in, so parallel
	// runs cannot reach each other's trees.
	workspacesLeaf = "workspaces"
)

// Request is the pure input to BuildPlan: every path the planner needs, already
// resolved by the cmd seam. HOME-relative fields (StateRW/StateRO/
// RuntimeLadder) are joined to Home inside the planner; the rest are absolute.
//
//   - Home          native home dir (absolute); tmpfs-blinded in "tmpfs" mode.
//   - ObserverDir   ~/.observer (absolute); bound rw — the observed-invariant.
//   - WorkspaceRoot the prepared workspace (absolute); bound rw, and --chdir'd.
//   - ObserverBin   os.Executable target (absolute); ro-bound so hooks resolve.
//   - ToolBinDirs   resolved tool bin dir + its real symlink-target dir (abs).
//   - StateRW       HOME-relative dirs/files the tool must WRITE (bind-try).
//   - StateRO       HOME-relative dirs the tool only reads (ro-bind-try).
//   - RuntimeLadder HOME-relative runtime probe dirs to ro-bind-try (nvm, etc).
//   - MaskPaths     absolute foreign-OS mounts to tmpfs-mask (A1, e.g. /mnt/c).
//   - HomeMode      "tmpfs" (default) | "readonly".
//   - ExtraRO       absolute config escape-hatch ro binds.
//   - ExtraRW       absolute config escape-hatch rw binds (EXPANDS authority).
type Request struct {
	Home          string
	ObserverDir   string
	WorkspaceRoot string
	ObserverBin   string
	ToolBinDirs   []string
	StateRW       []string
	StateRO       []string
	RuntimeLadder []string
	MaskPaths     []string
	HomeMode      string
	ExtraRO       []string
	ExtraRW       []string
}

// Plan is the composed, validated bwrap argv (everything from `--ro-bind / /`
// through `--die-with-parent`, excluding the bwrap executable and the inner
// argv). HomeMode records the normalized home mode for display. Build it with
// BuildPlan; render the full launch argv with Argv.
type Plan struct {
	pre      []string
	HomeMode string
}

// Argv returns the exec argv AFTER the bwrap executable: the ordered bwrap
// flags, the `--` end-of-options marker, then the inner argv. The caller
// prepends the resolved bwrap path (the pure planner does not know it — it is
// not in Request). `--` is placed IMMEDIATELY before inner. It returns nil when
// inner is empty or inner[0] begins with '-' (a flag-shaped program name would
// be swallowed by bwrap as one of its own options).
func (p Plan) Argv(inner []string) []string {
	if len(inner) == 0 {
		return nil
	}
	if strings.HasPrefix(inner[0], "-") {
		return nil
	}
	out := make([]string, 0, len(p.pre)+1+len(inner))
	out = append(out, p.pre...)
	out = append(out, argSep)
	out = append(out, inner...)
	return out
}

// BuildPlan composes the ordered bwrap flag sequence (§3) from req. The order
// is load-bearing and mutation-proofed: the $HOME tmpfs precedes every
// under-home bind, and the ~/.observer/workspaces tmpfs precedes the workspace
// rw bind — otherwise a punch-back would be shadowed by a later blinding.
//
// It returns an error rather than emit an unsafe argv when any resolved bind
// path is not absolute, contains a ".." segment, begins with '-', contains a
// NUL / whitespace / control character, or is "/" for an rw bind (a whole-root
// rw bind is forbidden; `--ro-bind / /` is the only whole-root and it is
// read-only). HOME-relative inputs are additionally rejected if absolute, ".",
// empty, or "..".
func BuildPlan(req Request) (Plan, error) {
	var pre []string
	add := func(tokens ...string) { pre = append(pre, tokens...) }

	// 1. Whole host root, read-only — gives /etc, /usr, runtimes, symlink
	//    targets for free. The single deliberate whole-root bind, and it is ro.
	add(flagROBind, "/", "/")
	add(flagProc, "/proc")
	add(flagDev, "/dev") // fresh devtmpfs the TUIs need; stdio already open.
	add(flagTmpfs, "/tmp")

	// 2. Blind $HOME (removes ~/.ssh, ~/.aws, every other tool's creds and
	//    every other repo) BEFORE any under-home punch-back. Omitted in
	//    "readonly" mode, where the ro-bind / / already makes home read-only.
	homeMode := homeModeTmpfs
	if req.HomeMode == homeModeReadonly {
		homeMode = homeModeReadonly
	}
	if homeMode == homeModeTmpfs {
		if err := validateAbs(req.Home, false); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: home: %w", err)
		}
		add(flagTmpfs, req.Home)
	}

	// 3. A1: mask foreign-OS mounts (/mnt/c-class) that ro-bind / / would leave
	//    readable — after the home tmpfs, BEFORE the punch-backs, so a tool or
	//    workspace path legitimately under a masked root is re-bound explicitly
	//    by the later ordered binds.
	for _, m := range req.MaskPaths {
		if err := validateAbs(m, false); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: mask path: %w", err)
		}
		add(flagTmpfs, m)
	}

	// 4. Derived read-only binaries + runtimes (ro-bind-try, tolerate missing),
	//    deduped by absolute source path (the ladder legitimately overlaps a
	//    tool bin dir): tool bin dirs, observer bin, runtime ladder, StateRO.
	seen := map[string]bool{}
	roTry := func(p string) error {
		if err := validateAbs(p, false); err != nil {
			return err
		}
		if seen[p] {
			return nil
		}
		seen[p] = true
		add(flagROBindTry, p, p)
		return nil
	}
	for _, d := range req.ToolBinDirs {
		if err := roTry(d); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: tool bin dir: %w", err)
		}
	}
	if req.ObserverBin != "" {
		if err := roTry(req.ObserverBin); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: observer bin: %w", err)
		}
	}
	for _, rel := range req.RuntimeLadder {
		p, err := joinHomeRel(req.Home, rel)
		if err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: runtime ladder %q: %w", rel, err)
		}
		if err := roTry(p); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: runtime ladder %q: %w", rel, err)
		}
	}
	for _, rel := range req.StateRO {
		p, err := joinHomeRel(req.Home, rel)
		if err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: state ro %q: %w", rel, err)
		}
		if err := roTry(p); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: state ro %q: %w", rel, err)
		}
	}

	// 5. ~/.observer rw — the observed-invariant (hook DB writes + inner
	//    launcher config read). Mutation proof #3 pins it rw.
	if err := validateAbs(req.ObserverDir, true); err != nil {
		return Plan{}, fmt.Errorf("sandbox.BuildPlan: observer dir: %w", err)
	}
	add(flagBind, req.ObserverDir, req.ObserverDir)

	// 6. Blind the managed-workspaces tree, THEN punch this run's workspace
	//    back in rw. The tmpfs MUST precede the workspace bind (mutation #2).
	wsParent := filepath.Join(req.ObserverDir, workspacesLeaf)
	add(flagTmpfs, wsParent)
	if err := validateAbs(req.WorkspaceRoot, true); err != nil {
		return Plan{}, fmt.Errorf("sandbox.BuildPlan: workspace root: %w", err)
	}
	add(flagBind, req.WorkspaceRoot, req.WorkspaceRoot)

	// 7. Per-tool writable state (bind-try, tolerate missing).
	for _, rel := range req.StateRW {
		p, err := joinHomeRel(req.Home, rel)
		if err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: state rw %q: %w", rel, err)
		}
		if err := validateAbs(p, true); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: state rw %q: %w", rel, err)
		}
		add(flagBindTry, p, p)
	}

	// 8. Config escape hatches: ro first, then authority-EXPANDING rw.
	for _, p := range req.ExtraRO {
		if err := validateAbs(p, false); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: extra ro bind: %w", err)
		}
		add(flagROBind, p, p)
	}
	for _, p := range req.ExtraRW {
		if err := validateAbs(p, true); err != nil {
			return Plan{}, fmt.Errorf("sandbox.BuildPlan: extra rw bind: %w", err)
		}
		add(flagBind, p, p)
	}

	// 9. Enter the workspace and die with the daemon.
	add(flagChdir, req.WorkspaceRoot)
	add(flagDieWithParent)

	return Plan{pre: pre, HomeMode: homeMode}, nil
}

// joinHomeRel validates a HOME-relative input and joins it to home, then
// validates the resulting absolute path. The rel is rejected if absolute,
// empty, ".", contains a ".." segment, begins with '-', or carries a NUL /
// whitespace / control character — so it can neither escape home nor smuggle a
// flag.
func joinHomeRel(home, rel string) (string, error) {
	if err := validateRel(rel); err != nil {
		return "", err
	}
	p := filepath.Join(home, rel)
	if err := validateAbs(p, false); err != nil {
		return "", err
	}
	return p, nil
}

// validateRel guards a HOME-relative path before it is joined to home.
func validateRel(rel string) error {
	if rel == "" || rel == "." {
		return fmt.Errorf("empty HOME-relative path")
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("%q must be HOME-relative, not absolute", rel)
	}
	if strings.HasPrefix(rel, "-") {
		return fmt.Errorf("%q must not begin with '-'", rel)
	}
	if hasDotDot(rel) {
		return fmt.Errorf("%q must not contain a '..' segment", rel)
	}
	if i := badCharIndex(rel); i >= 0 {
		return fmt.Errorf("%q contains a NUL, whitespace, or control character", rel)
	}
	return nil
}

// validateAbs guards a resolved bind path. rw marks a writable bind, for which
// the whole-root "/" is additionally forbidden.
func validateAbs(p string, rw bool) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("%q must not begin with '-'", p)
	}
	if i := badCharIndex(p); i >= 0 {
		return fmt.Errorf("%q contains a NUL, whitespace, or control character", p)
	}
	if hasDotDot(p) {
		return fmt.Errorf("%q must not contain a '..' segment", p)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%q must be absolute", p)
	}
	if rw && filepath.Clean(p) == "/" {
		return fmt.Errorf("a whole-root rw bind (%q) is forbidden", p)
	}
	return nil
}

// hasDotDot reports whether any '/'-separated segment of p is "..".
func hasDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// badCharIndex returns the index of the first NUL, whitespace, or control
// character in s (space, tab, newline, and every rune <= 0x20 or == 0x7f), or
// -1 when none is present. Such characters break the single-argv-token contract
// and could smuggle a second flag past a naive forwarder.
func badCharIndex(s string) int {
	for i, r := range s {
		if r <= ' ' || r == 0x7f {
			return i
		}
	}
	return -1
}
