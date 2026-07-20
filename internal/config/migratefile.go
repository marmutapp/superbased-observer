package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/marmutapp/superbased-observer/internal/config/migrate"
)

// MigrateFile is the I/O boundary for the config auto-migration rail:
// it reads the config.toml at path, runs the pure text migrator
// (internal/config/migrate), and — only when a real migration happened
// and nothing was skipped — writes the result back through the shared
// atomic writer (a path+".bak" backup, then temp-file + rename).
//
// It is safe to call unconditionally at daemon startup and from the
// `observer config migrate` CLI: a missing file, a pristine file, or an
// already-current file all return a zero-change Result and touch
// nothing. A file the migrator cannot edit safely comes back
// Skipped=true, again untouched — a migration never corrupts the file.
//
// path must be the already-resolved global config path (callers use
// ResolveGlobalPath). MigrateFile is the ONLY writer on this rail, so
// backup + atomicity semantics match WriteToml exactly.
func MigrateFile(path string) (migrate.Result, error) {
	if path == "" {
		return migrate.Result{}, errors.New("config.MigrateFile: empty path")
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return migrate.Result{}, nil
	}
	if err != nil {
		return migrate.Result{}, fmt.Errorf("config.MigrateFile: read %s: %w", path, err)
	}
	res, err := migrate.Apply(string(body))
	if err != nil {
		return res, fmt.Errorf("config.MigrateFile: apply %s: %w", path, err)
	}
	if res.Migrated && !res.Skipped {
		if err := writeBytesAtomic(path, []byte(res.Text)); err != nil {
			return res, fmt.Errorf("config.MigrateFile: write %s: %w", path, err)
		}
	}
	return res, nil
}
