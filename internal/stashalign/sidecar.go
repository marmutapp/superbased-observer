package stashalign

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SidecarFilename is the well-known sidecar file written by the proxy
// to advertise its active stash dir. Lives directly under the user's
// observer home (typically `~/.observer/`), NOT inside the stash dir
// itself — the MCP server reads it before it knows where the stash
// is. Hidden-prefixed to keep ls(1) output tidy.
const SidecarFilename = ".proxy-stash-dir.txt"

// WriteSidecar persists stashDir as the active proxy-side stash
// directory under homeDir. Creates homeDir if it doesn't exist
// already (matching observer's general "create on first use"
// posture). Returns an error only for I/O failures — callers should
// log+continue rather than abort.
//
// Atomic write via temp+rename so a concurrent ReadSidecar never
// sees a partial path. Last-writer-wins: multiple proxies on the
// same host overwrite each other, which is acceptable since the
// detection is advisory.
func WriteSidecar(homeDir, stashDir string) error {
	if homeDir == "" {
		return errors.New("stashalign.WriteSidecar: homeDir is required")
	}
	if stashDir == "" {
		return errors.New("stashalign.WriteSidecar: stashDir is required")
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return fmt.Errorf("stashalign.WriteSidecar: mkdir: %w", err)
	}
	final := filepath.Join(homeDir, SidecarFilename)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(stashDir+"\n"), 0o600); err != nil {
		return fmt.Errorf("stashalign.WriteSidecar: write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stashalign.WriteSidecar: rename: %w", err)
	}
	return nil
}

// ReadSidecar returns the stash dir advertised by the proxy-side
// WriteSidecar, or empty string when the file doesn't exist
// (uninitialized state — not an error).
//
// I/O errors other than "missing" propagate so the MCP server can
// distinguish "no proxy ran yet" from "permissions are broken." The
// trailing newline appended by WriteSidecar is stripped; leading and
// trailing whitespace are stripped defensively.
func ReadSidecar(homeDir string) (string, error) {
	if homeDir == "" {
		return "", errors.New("stashalign.ReadSidecar: homeDir is required")
	}
	final := filepath.Join(homeDir, SidecarFilename)
	data, err := os.ReadFile(final)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stashalign.ReadSidecar: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// DefaultObserverHome returns the observer user-state directory
// (typically `~/.observer`) by joining [os.UserHomeDir] with the
// well-known `.observer` segment. Returns an error when
// [os.UserHomeDir] does (rare — would also break the config loader).
// Exported so callers wiring stash-dir advisories from `observer
// proxy` / `observer serve` can compute the sidecar location
// without duplicating the path constant.
func DefaultObserverHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("stashalign.DefaultObserverHome: %w", err)
	}
	return filepath.Join(home, ".observer"), nil
}

// SidecarPath reports the absolute path the sidecar lives at given
// homeDir. Exported for diagnostics — useful in startup log messages
// pointing operators at where to look.
func SidecarPath(homeDir string) string {
	return filepath.Join(homeDir, SidecarFilename)
}

// CompareDirs decides whether two paths point at the same on-disk
// location. Returns (true, "") on match. On mismatch returns
// (false, recoveryHint) where the hint names the specific operator
// remedy. Resolution accounts for trailing slashes and symlinks via
// [filepath.EvalSymlinks]; absent paths fall back to lexical equality
// after Clean.
//
// Used by `observer serve` to validate its own configured stash dir
// against the proxy's. Same-path means everything works; different
// paths mean retrieve_stashed will see misses.
func CompareDirs(mcpDir, proxyDir string) (bool, string) {
	if mcpDir == "" || proxyDir == "" {
		return false, "one side has no stash dir configured; ensure both proxy and MCP server enable [compression.conversation.stash]"
	}
	mcpResolved := resolveDir(mcpDir)
	proxyResolved := resolveDir(proxyDir)
	if mcpResolved == proxyResolved {
		return true, ""
	}
	return false, fmt.Sprintf(
		"proxy is writing to %s but this MCP server reads from %s — "+
			"point both at the same path via [compression.conversation.stash].Dir, "+
			"OR start both processes with the same --config file",
		proxyDir, mcpDir,
	)
}

// resolveDir best-effort canonicalises a path: Clean + Abs +
// EvalSymlinks. Stat failures (path doesn't exist yet) fall back to
// the Clean+Abs form. Used by CompareDirs to compare paths that
// differ only in trailing slash, "..", or symlink intermediates.
func resolveDir(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}
