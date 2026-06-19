package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// WriteToml saves cfg to path with a .bak fallback (Option A from the
// settings planning doc — comments lost on save, prior version
// preserved). This is THE config write owner: the dashboard settings
// seam and the `observer profile` / `observer config` CLI commands all
// funnel through it (plan §2.3.5 — one owner, two front doors), so
// backup semantics and atomicity can't drift between surfaces.
//
// Steps:
//  1. If path exists, copy it to path+".bak" so the user can recover
//     hand-written comments.
//  2. Marshal cfg to TOML in a temp file in the same directory (atomic
//     rename requires same filesystem).
//  3. os.Rename to path. If this fails, the .bak from step 1 is the
//     authoritative backup.
func WriteToml(path string, cfg Config) error {
	return writeTOMLAtomic(path, cfg)
}

// writeTOMLAtomic is the underlying generic writer: same .bak +
// temp-file + rename mechanics for any marshalable document (the full
// Config for the global file; the allow-listed overlay doc for
// project files).
func writeTOMLAtomic(path string, doc any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", existing, 0o644); err != nil { //nolint:gosec // G306: backup of the non-secret config.toml; mirrors the original's readable perms.
			return fmt.Errorf("write .bak: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read current config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if rename failed.
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(doc); err != nil {
		tmp.Close()
		return fmt.Errorf("marshal toml: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
