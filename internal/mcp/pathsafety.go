package mcp

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Path-safety helpers for the v1.7.8 get_file tool. Together they
// implement the V7-13 Gap 4 defense layers (project-root containment,
// allow-extension allow-list, deny-glob list, response-size cap is
// enforced in the tool handler not here).
//
// The helpers are package-private; only the get_file tool uses them
// today. Future V7-12 tools (get_symbols, get_relations) will share
// the same helpers as their access path is similarly file-scoped.

// errPathOutsideRoot is the canonical deny reason when symlink-resolved
// path doesn't share the project_root prefix. Exported message stays
// stable across releases — used by client error-pattern matching.
var (
	errPathOutsideRoot = errors.New("path outside project_root")
	errFileNotFound    = errors.New("file not found")
)

// safeResolvePath joins root + p, expands symlinks, and verifies the
// resolved path is contained under the resolved project root. Returns
// the resolved absolute path on success.
//
// Containment is checked AFTER EvalSymlinks so a symlink under the
// project tree pointing outside the tree is correctly denied. This is
// the standard symlink-escape defense; same approach as
// cmd/observer/init.go for hook-script path resolution.
//
// TOCTOU: a same-host attacker could swap a symlink between this
// resolution and os.Open. Documented as accepted risk in
// docs/mcp-get-file-reference.md — the MCP server runs in the same
// trust domain as the agent so an attacker with that capability has
// equivalent access via the shell tool.
func safeResolvePath(root, p string) (string, error) {
	if root == "" {
		return "", errors.New("project_root is required")
	}
	if p == "" {
		return "", errors.New("path is required")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve project_root: %w", err)
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		// Project root must exist; otherwise the containment check has
		// no anchor.
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("project_root does not exist: %s", rootAbs)
		}
		return "", fmt.Errorf("resolve project_root symlinks: %w", err)
	}

	var joined string
	if filepath.IsAbs(p) {
		joined = p
	} else {
		joined = filepath.Join(rootResolved, p)
	}
	pathAbs, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	pathResolved, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errFileNotFound
		}
		return "", fmt.Errorf("resolve path symlinks: %w", err)
	}

	if !isUnder(pathResolved, rootResolved) {
		return "", errPathOutsideRoot
	}
	return pathResolved, nil
}

// isUnder returns true when child equals parent or is contained beneath
// parent. Both paths must be cleaned and absolute (caller's responsibility).
// Uses filepath.Separator to handle Windows + POSIX uniformly.
func isUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}

// extensionAllowed reports whether the file's extension appears in
// allowed (case-insensitive comparison on the last `.<ext>` segment).
// An empty `allowed` list disables the check (operator escape hatch).
// Paths with no extension are denied unless the allow-list is empty.
func extensionAllowed(absPath string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	ext := filepath.Ext(absPath) // includes the leading dot, e.g. ".ts"
	if ext == "" {
		return false
	}
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	for _, want := range allowed {
		if strings.EqualFold(strings.TrimPrefix(want, "."), ext) {
			return true
		}
	}
	return false
}

// matchesAny reports whether relPath (project-relative, slash-separated)
// matches any of the patterns. Supported pattern syntax is a small
// subset chosen to cover V7-13 Gap 4's default deny list without
// pulling a glob dependency:
//
//   - `*` matches any sequence of non-slash chars in a single segment.
//   - `?` matches one non-slash char.
//   - `<dir>/**` matches any path under `<dir>` (recursive containment).
//
// The pattern is matched against BOTH the full relative path AND the
// basename; the first match wins. This makes `.env*` work on
// `subdir/.env` AND `.env`, and makes `.git/**` work as a prefix.
//
// Patterns must use forward slashes. Callers convert with
// filepath.ToSlash.
func matchesAny(relPath string, patterns []string) bool {
	rel := filepath.ToSlash(relPath)
	base := path.Base(rel)
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		pat = filepath.ToSlash(pat)
		if strings.HasSuffix(pat, "/**") {
			prefix := strings.TrimSuffix(pat, "/**")
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// hasUnsupportedGlobSyntax reports whether pattern uses syntax
// outside the supported subset (character classes `[...]`,
// brace alternation `{a,b}`, escape sequences `\x`). Used at
// config.Load to warn operators about silently-dead patterns.
func hasUnsupportedGlobSyntax(pattern string) bool {
	if strings.ContainsAny(pattern, "[]{}\\") {
		return true
	}
	return false
}
