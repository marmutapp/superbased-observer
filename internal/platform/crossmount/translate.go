package crossmount

import "github.com/marmutapp/superbased-observer/internal/platform/pathnorm"

// TranslateForeignPath converts a path expressed in a foreign-OS
// convention to the equivalent path reachable from the running
// process. Historically this only handled the Windows drive-letter
// case (`C:\foo` → `/mnt/c/foo`); v1.6.29 routed it through
// pathnorm.Normalize so callers automatically pick up handling for:
//
//   - matched surrounding quotes (`'C:\foo'`, `"D:/bar"`)
//   - file:// URIs (`file:///D:/foo`, percent-encoded)
//   - Windows extended-length prefix (`\\?\C:\foo`)
//   - UNC-to-WSL (`\\wsl.localhost\<distro>\...`,
//     `\\wsl$\<distro>\...`) — only when the embedded distro
//     matches the current $WSL_DISTRO_NAME
//   - Git Bash drive prefix (`/c/Users/foo` → `/mnt/c/Users/foo`)
//   - tilde expansion (`~/foo`, bare `~`)
//
// Same never-fail contract as pathnorm.Normalize — unrecognised
// inputs return unchanged rather than erroring out.
//
// Existing call sites in the adapters keep working with no change —
// the original drive-letter behaviour is the strict subset that
// pathnorm.FormatWindowsDrive handles. See
// internal/platform/pathnorm/doc.go for the full pipeline and
// pathnorm_test.go for the tested format matrix.
func TranslateForeignPath(p string) string {
	return pathnorm.Normalize(p)
}
