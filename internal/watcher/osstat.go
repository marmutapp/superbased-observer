package watcher

import (
	"io/fs"
	"os"
)

// osStat is an indirection over os.Stat so callers can mock it in tests if
// needed. Right now it just delegates.
func osStat(path string) (fs.FileInfo, error) { return os.Stat(path) }
