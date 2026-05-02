package crossmount

import (
	"os"
	"path/filepath"
	"runtime"
)

// OS tags emitted on HomeRoot.
const (
	OSWindows = "windows"
	OSLinux   = "linux"
	OSDarwin  = "darwin"
)

// HomeRoot is one candidate $HOME-equivalent directory.
type HomeRoot struct {
	// Path is the absolute filesystem path to the home directory, in
	// whatever form is reachable from the running process (e.g.
	// "/mnt/c/Users/auzy_" on WSL2, `\\wsl.localhost\Ubuntu\home\me` on
	// Windows).
	Path string
	// OS tags whose conventions the home follows: "windows", "linux", or
	// "darwin". This is the LOGICAL OS of the home — not necessarily
	// the same as runtime.GOOS. A native home on a Linux host has
	// OS=="linux"; a /mnt/c/Users/<u> entry on the same host has
	// OS=="windows".
	OS string
	// Origin describes where this candidate came from for diagnostic
	// logging: "native", "wsl-mnt:<user>", or "wslhost:<distro>/<user>".
	Origin string
}

// detector is the test seam — production code uses defaultDetector(),
// tests construct one with fakes for runtimeOS / nativeHome / statDir /
// readDir to exercise both bridge directions on any host.
type detector struct {
	runtimeOS  string
	nativeHome func() (string, error)
	statDir    func(path string) bool
	readDir    func(path string) ([]string, error)
}

func defaultDetector() *detector {
	return &detector{
		runtimeOS:  runtime.GOOS,
		nativeHome: os.UserHomeDir,
		statDir:    isExistingDir,
		readDir:    readDirNames,
	}
}

// AllHomes returns the native home (when resolvable) plus every
// auto-detected cross-mount home. Order: native first, then extras in
// directory-listing order (which on most filesystems is creation
// order, sometimes alphabetical — callers must not rely on it).
//
// Safe to call on any platform; never errors.
func AllHomes() []HomeRoot {
	return defaultDetector().allHomes()
}

// ExtraHomes returns only the auto-detected cross-mount homes,
// excluding the native home. Useful for diagnostic logging that
// surfaces what the bridge picked up.
func ExtraHomes() []HomeRoot {
	return defaultDetector().extraHomes()
}

func (d *detector) allHomes() []HomeRoot {
	var all []HomeRoot
	if home, err := d.nativeHome(); err == nil && home != "" {
		all = append(all, HomeRoot{
			Path:   home,
			OS:     d.runtimeOS,
			Origin: "native",
		})
	}
	all = append(all, d.extraHomes()...)
	return all
}

func (d *detector) extraHomes() []HomeRoot {
	switch d.runtimeOS {
	case OSLinux:
		return d.wslWindowsHomes()
	case OSWindows:
		return d.windowsWSLHomes()
	}
	return nil
}

// wslWindowsHomes enumerates Windows user homes reachable from a WSL2
// host via the /mnt/c bind. Returns nil when /mnt/c/Users is not a
// directory (pure Linux host, or WSL2 instance without the C: mount).
//
// Per design: NO name-based filtering. The watcher's anyDirExists +
// per-adapter subpath check turns inert candidates into no-ops; we'd
// rather risk an extra inert home than skip a legitimate user dir.
// We do still require the candidate is itself a directory, so files
// like desktop.ini get skipped (filepath.WalkDir would error on them
// otherwise).
func (d *detector) wslWindowsHomes() []HomeRoot {
	const root = "/mnt/c/Users"
	if !d.statDir(root) {
		return nil
	}
	names, err := d.readDir(root)
	if err != nil {
		return nil
	}
	var out []HomeRoot
	for _, name := range names {
		path := filepath.Join(root, name)
		if !d.statDir(path) {
			continue
		}
		out = append(out, HomeRoot{
			Path:   path,
			OS:     OSWindows,
			Origin: "wsl-mnt:" + name,
		})
	}
	return out
}

// windowsWSLHomes enumerates Linux user homes reachable from a Windows
// host via \\wsl.localhost\<distro>\home\<user>. Returns nil when
// \\wsl.localhost\ is not enumerable (no WSL installed, or no distros
// running). Best-effort: distros are typically reachable only when the
// distro is running or recently used; that's an acceptable limitation.
func (d *detector) windowsWSLHomes() []HomeRoot {
	const root = `\\wsl.localhost\`
	if !d.statDir(root) {
		return nil
	}
	distros, err := d.readDir(root)
	if err != nil {
		return nil
	}
	var out []HomeRoot
	for _, distro := range distros {
		homeDir := filepath.Join(root, distro, "home")
		if !d.statDir(homeDir) {
			continue
		}
		users, err := d.readDir(homeDir)
		if err != nil {
			continue
		}
		for _, user := range users {
			userPath := filepath.Join(homeDir, user)
			if !d.statDir(userPath) {
				continue
			}
			out = append(out, HomeRoot{
				Path:   userPath,
				OS:     OSLinux,
				Origin: "wslhost:" + distro + "/" + user,
			})
		}
	}
	return out
}

func isExistingDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func readDirNames(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
