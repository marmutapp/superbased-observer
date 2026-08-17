package termsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrProjectRootDenied is returned by ValidateProjectRoot when a requested
// project root is not accepted: it is relative, a UNC/device/network path,
// does not exist, is not a directory, or does not canonicalize to (or under)
// an operator-configured allowed root. It EXPANDS execution authority, so the
// validator is conservative and fails closed.
var ErrProjectRootDenied = errors.New("termsvc: project root not permitted")

// ErrManagedWorkspaceDenied is returned by ValidateManagedWorkspace when a
// prepared B9 sandbox workspace path does not canonicalize to (or under) the
// daemon's configured managed-workspace root, or the managed root itself is
// unset (a misconfiguration — treated as fail-closed, never a wildcard
// accept). It is the narrower, B9-specific sibling of ErrProjectRootDenied:
// ValidateProjectRoot gates WHICH repo may be copied FROM, this gates where a
// freshly prepared managed workspace may land.
var ErrManagedWorkspaceDenied = errors.New("termsvc: sandbox workspace path not permitted")

// ValidateProjectRoot authorizes a client-influenced project_root against the
// operator-configured allow-list (plan §F1 project-root authorization). It is
// deliberately strict:
//
//   - An empty requested root returns ("", nil): the launcher's own default
//     cwd is used, which is always safe.
//   - A non-empty root must be absolute, must NOT be a UNC/device/network path
//     (\\host\share, \\.\dev, \\?\..., //host, or contain a NUL), must exist
//     as a directory, and — after canonicalization via real filesystem
//     identity (filepath.EvalSymlinks, which resolves every symlink component
//     so a symlink/junction escape resolves to its real target) — must equal
//     or be a descendant of a canonicalized allowed root.
//   - An empty allow-list denies every non-empty root (deny-all): a
//     project_root is only ever accepted when the operator has listed one.
//
// It returns the CANONICAL path to hand the spawner (real identity, symlinks
// resolved) so there is no TOCTOU gap between the check and the spawn beyond
// the caller invoking this immediately before Launcher.Spawn (which the
// service does — re-validation at spawn, plan §F1).
func ValidateProjectRoot(requested string, allowedRoots []string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return "", nil
	}
	if err := rejectDangerousPath(requested); err != nil {
		return "", err
	}
	if !filepath.IsAbs(requested) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrProjectRootDenied, requested)
	}
	canonical, err := canonicalDir(requested)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrProjectRootDenied, err)
	}
	if len(allowedRoots) == 0 {
		return "", fmt.Errorf("%w: no allowed_project_roots are configured", ErrProjectRootDenied)
	}
	for _, root := range allowedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		allowedCanon, aerr := canonicalDir(root)
		if aerr != nil {
			// A misconfigured allow-list entry is skipped, not fatal — another
			// entry may still match.
			continue
		}
		if isUnderOrEqual(canonical, allowedCanon) {
			return canonical, nil
		}
	}
	return "", fmt.Errorf("%w: %q is not within any allowed_project_roots entry", ErrProjectRootDenied, requested)
}

// ValidateManagedWorkspace authorizes a B9 sandbox-prepared workspace path
// (plan §4/§5): the prepared directory must canonicalize strictly under the
// daemon's configured managed-workspace root (managedRoot, e.g.
// `<observerDir>/workspaces`). It reuses the SAME canonicalization
// (canonicalDir — filepath.Abs + EvalSymlinks, requiring the path to exist as
// a directory) and containment check (isUnderOrEqual) that ValidateProjectRoot
// uses, so the two validators stay behaviourally aligned without either
// weakening the other.
//
// An empty managedRoot is treated as a misconfiguration and rejected
// (fail-closed, plan §7): there is no sense in which "no managed root
// configured" should accept an arbitrary path. Both path and managedRoot are
// canonicalized independently so a misconfigured or symlinked managedRoot
// still resolves to its real identity before the containment check runs.
//
// It returns the canonical path for the caller to adopt as the run's
// Dir/ProjectRootHash source, matching ValidateProjectRoot's contract.
func ValidateManagedWorkspace(path, managedRoot string) (string, error) {
	if strings.TrimSpace(managedRoot) == "" {
		return "", fmt.Errorf("%w: no managed workspace root is configured", ErrManagedWorkspaceDenied)
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: empty workspace path", ErrManagedWorkspaceDenied)
	}
	if err := rejectDangerousPath(path); err != nil {
		return "", fmt.Errorf("%w: %w", ErrManagedWorkspaceDenied, err)
	}
	canonical, err := canonicalDir(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrManagedWorkspaceDenied, err)
	}
	rootCanonical, err := canonicalDir(managedRoot)
	if err != nil {
		return "", fmt.Errorf("%w: managed root: %w", ErrManagedWorkspaceDenied, err)
	}
	if !isUnderOrEqual(canonical, rootCanonical) {
		return "", fmt.Errorf("%w: %q is not within the managed workspace root", ErrManagedWorkspaceDenied, path)
	}
	return canonical, nil
}

// rejectDangerousPath rejects UNC / device / network / NUL-bearing paths
// before any filesystem call.
func rejectDangerousPath(p string) error {
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: path contains a NUL byte", ErrProjectRootDenied)
	}
	// Windows UNC (\\host\share) and device (\\.\, \\?\) namespaces, and the
	// forward-slash UNC form (//host). filepath.IsAbs would accept some of
	// these; reject them outright.
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return fmt.Errorf("%w: UNC/network paths are not permitted", ErrProjectRootDenied)
	}
	if strings.Contains(p, `\\.\`) || strings.Contains(p, `\\?\`) {
		return fmt.Errorf("%w: device-namespace paths are not permitted", ErrProjectRootDenied)
	}
	return nil
}

// canonicalDir resolves p to its real filesystem identity and asserts it is an
// existing directory. EvalSymlinks resolves every symlink component, so a
// symlink escape resolves to its true target (which the allow-list check then
// rejects if it lands outside).
func canonicalDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve real path (must exist): %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", real)
	}
	return real, nil
}

// isUnderOrEqual reports whether child is parent or a descendant of it. Both
// must already be canonical (absolute, symlinks resolved). It uses
// filepath.Rel and rejects any result that escapes with "..".
func isUnderOrEqual(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// A descendant's relative path never starts with ".." and is never
	// absolute.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
