package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validateAbsPath requires path to be a non-empty absolute path with no
// ".." traversal segment and no NUL/whitespace/control character
// anywhere in it. It does not touch the filesystem — canonicalizing
// symlinks is the caller's job (see doc.go / ValidateManagedWorkspace).
func validateAbsPath(field, path string) error {
	if path == "" {
		return fmt.Errorf("workspace: %s must not be empty", field)
	}
	if err := validateNoControlOrWhitespace(field, path); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("workspace: %s %q must be an absolute path", field, path)
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if seg == ".." {
			return fmt.Errorf("workspace: %s %q must not contain a %q segment", field, path, "..")
		}
	}
	return nil
}

// validateCleanToken requires tok to be a non-empty single path segment
// safe to place immediately after a flag in a composed git argv (e.g.
// `-b <tok>`, a managed-root ID directory name, or a derived repo leaf):
// no path separators, no "."/".." sentinel, no leading '-' (flag-
// injection guard, mirrors internal/integration.ModelLaunch), and no
// whitespace/control characters.
func validateCleanToken(field, tok string) error {
	if tok == "" {
		return fmt.Errorf("workspace: %s must not be empty", field)
	}
	if err := validateNoControlOrWhitespace(field, tok); err != nil {
		return err
	}
	if strings.HasPrefix(tok, "-") {
		return fmt.Errorf("workspace: %s %q must not begin with '-'", field, tok)
	}
	if tok == "." || tok == ".." {
		return fmt.Errorf("workspace: %s %q must not be %q", field, tok, tok)
	}
	if strings.ContainsAny(tok, "/\\") {
		return fmt.Errorf("workspace: %s %q must not contain a path separator", field, tok)
	}
	return nil
}

// validateNoControlOrWhitespace rejects NUL, whitespace, and other
// control characters — the same class internal/integration.ModelLaunch
// rejects for a model argv token.
func validateNoControlOrWhitespace(field, s string) error {
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("workspace: %s %q contains whitespace or a control character", field, s)
		}
	}
	return nil
}

// ValidateManagedWorkspace requires that path lies strictly under
// managedRoot, given two already-cleaned absolute paths (filepath.Clean
// applied, no symlink resolution attempted here).
//
// This is the pure half of a two-step check described in
// docs/plans/b9-sandboxed-terminals-implementation-plan-2026-08-08.md
// §4: the CALLER (U5, cmd/observer) must filepath.EvalSymlinks (or
// equivalent) both paths first, once the workspace actually exists on
// disk — a symlink planted inside a workspace could otherwise point
// outside managedRoot, and this pure function has no filesystem access
// to catch that. It mirrors the existing canonicalDir/isUnderOrEqual
// discipline behind ValidateProjectRoot, applied narrower here (a
// workspace path vs the daemon's own managed root, not an arbitrary
// operator allow-list).
func ValidateManagedWorkspace(path, managedRoot string) error {
	if err := validateAbsPath("path", path); err != nil {
		return err
	}
	if err := validateAbsPath("managedRoot", managedRoot); err != nil {
		return err
	}
	cp := filepath.Clean(path)
	cr := filepath.Clean(managedRoot)
	rel, err := filepath.Rel(cr, cp)
	if err != nil {
		return fmt.Errorf("workspace: path %q is not under managed root %q: %w", path, managedRoot, err)
	}
	if rel == "." {
		// The managed root itself is not a valid workspace directory — a
		// workspace always lives at least one segment below it
		// (<root>/<id>/<leaf>).
		return fmt.Errorf("workspace: path %q equals managed root %q, not a workspace under it", path, managedRoot)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workspace: path %q escapes managed root %q", path, managedRoot)
	}
	return nil
}
