package crossmount

import (
	"errors"
	"path/filepath"
	"testing"
)

// fakeFS holds a flat dir set + a flat file set; statDir returns true
// only for entries in dirs, readDir returns names listed under each
// parent. Lets us stage cross-mount layouts without touching the host
// filesystem.
type fakeFS struct {
	dirs     map[string]bool
	listings map[string][]string
}

func (f *fakeFS) statDir(p string) bool { return f.dirs[p] }
func (f *fakeFS) readDir(p string) ([]string, error) {
	v, ok := f.listings[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return v, nil
}

func TestAllHomesNativeOnlyPureLinux(t *testing.T) {
	t.Parallel()
	// Pure Linux: /mnt/c/Users not present. Only the native home.
	fs := &fakeFS{dirs: map[string]bool{}}
	d := &detector{
		runtimeOS:  OSLinux,
		nativeHome: func() (string, error) { return "/home/me", nil },
		statDir:    fs.statDir,
		readDir:    fs.readDir,
	}
	got := d.allHomes()
	if len(got) != 1 {
		t.Fatalf("len: got %d want 1: %+v", len(got), got)
	}
	if got[0].Path != "/home/me" || got[0].OS != OSLinux || got[0].Origin != "native" {
		t.Errorf("native home wrong: %+v", got[0])
	}
}

func TestAllHomesWSL2EnumeratesEveryWindowsUser(t *testing.T) {
	t.Parallel()
	// WSL2: /mnt/c/Users has 4 entries — 2 real user dirs, 1 service
	// account, 1 file (desktop.ini). Per design we DON'T name-filter,
	// so all dirs come through; the file is skipped because statDir
	// returns false for it.
	fs := &fakeFS{
		dirs: map[string]bool{
			"/mnt/c/Users":              true,
			"/mnt/c/Users/auzy_":        true,
			"/mnt/c/Users/Public":       true,
			"/mnt/c/Users/CodexSandbox": true,
			// desktop.ini is a FILE, not a dir — must be filtered.
		},
		listings: map[string][]string{
			"/mnt/c/Users": {"auzy_", "Public", "CodexSandbox", "desktop.ini"},
		},
	}
	d := &detector{
		runtimeOS:  OSLinux,
		nativeHome: func() (string, error) { return "/home/me", nil },
		statDir:    fs.statDir,
		readDir:    fs.readDir,
	}
	got := d.allHomes()
	if len(got) != 4 {
		t.Fatalf("len: got %d want 4 (native + 3 user dirs): %+v", len(got), got)
	}
	if got[0].Origin != "native" {
		t.Errorf("native must come first: got %+v", got[0])
	}
	want := map[string]string{
		"/mnt/c/Users/auzy_":        "wsl-mnt:auzy_",
		"/mnt/c/Users/Public":       "wsl-mnt:Public",
		"/mnt/c/Users/CodexSandbox": "wsl-mnt:CodexSandbox",
	}
	for _, h := range got[1:] {
		if h.OS != OSWindows {
			t.Errorf("extra home OS: got %q want %q for %s", h.OS, OSWindows, h.Path)
		}
		if want[h.Path] != h.Origin {
			t.Errorf("origin for %s: got %q want %q", h.Path, h.Origin, want[h.Path])
		}
	}
}

func TestAllHomesWSL2NoMountReturnsNativeOnly(t *testing.T) {
	t.Parallel()
	// /mnt/c/Users not present (WSL instance without the C: mount, or
	// pure Linux host).
	fs := &fakeFS{dirs: map[string]bool{}}
	d := &detector{
		runtimeOS:  OSLinux,
		nativeHome: func() (string, error) { return "/home/me", nil },
		statDir:    fs.statDir,
		readDir:    fs.readDir,
	}
	got := d.allHomes()
	if len(got) != 1 {
		t.Errorf("expected native only, got %+v", got)
	}
}

func TestAllHomesWindowsEnumeratesEveryWSLDistroUser(t *testing.T) {
	t.Parallel()
	root := `\\wsl.localhost\`
	ub := filepath.Join(root, "Ubuntu")
	dn := filepath.Join(root, "Debian")
	fs := &fakeFS{
		dirs: map[string]bool{
			root:                                 true,
			filepath.Join(ub, "home"):            true,
			filepath.Join(ub, "home", "marmu"):   true,
			filepath.Join(ub, "home", "santosh"): true,
			filepath.Join(dn, "home"):            true,
			filepath.Join(dn, "home", "deploy"):  true,
		},
		listings: map[string][]string{
			root:                      {"Ubuntu", "Debian"},
			filepath.Join(ub, "home"): {"marmu", "santosh"},
			filepath.Join(dn, "home"): {"deploy"},
		},
	}
	d := &detector{
		runtimeOS:  OSWindows,
		nativeHome: func() (string, error) { return `C:\Users\me`, nil },
		statDir:    fs.statDir,
		readDir:    fs.readDir,
	}
	got := d.allHomes()
	if len(got) != 4 {
		t.Fatalf("len: got %d want 4 (native + 3 wsl users): %+v", len(got), got)
	}
	for _, h := range got[1:] {
		if h.OS != OSLinux {
			t.Errorf("wslhost home OS: got %q want %q", h.OS, OSLinux)
		}
	}
}

func TestAllHomesWindowsNoWSLReturnsNativeOnly(t *testing.T) {
	t.Parallel()
	fs := &fakeFS{dirs: map[string]bool{}}
	d := &detector{
		runtimeOS:  OSWindows,
		nativeHome: func() (string, error) { return `C:\Users\me`, nil },
		statDir:    fs.statDir,
		readDir:    fs.readDir,
	}
	got := d.allHomes()
	if len(got) != 1 || got[0].OS != OSWindows {
		t.Errorf("expected native windows only, got %+v", got)
	}
}

func TestAllHomesDarwinReturnsNativeOnly(t *testing.T) {
	t.Parallel()
	d := &detector{
		runtimeOS:  OSDarwin,
		nativeHome: func() (string, error) { return "/Users/me", nil },
		statDir:    func(string) bool { return false },
		readDir:    func(string) ([]string, error) { return nil, errors.New("noop") },
	}
	got := d.allHomes()
	if len(got) != 1 || got[0].OS != OSDarwin {
		t.Errorf("expected native darwin only, got %+v", got)
	}
}

func TestAllHomesNativeHomeFailureStillReturnsExtras(t *testing.T) {
	t.Parallel()
	// nativeHome errors (rare; e.g. UserHomeDir fails on a stripped
	// container). The cross-mount enumeration still proceeds.
	fs := &fakeFS{
		dirs: map[string]bool{
			"/mnt/c/Users":       true,
			"/mnt/c/Users/auzy_": true,
		},
		listings: map[string][]string{
			"/mnt/c/Users": {"auzy_"},
		},
	}
	d := &detector{
		runtimeOS:  OSLinux,
		nativeHome: func() (string, error) { return "", errors.New("no home") },
		statDir:    fs.statDir,
		readDir:    fs.readDir,
	}
	got := d.allHomes()
	if len(got) != 1 {
		t.Fatalf("want 1 (extra only, native unresolvable): got %+v", got)
	}
	if got[0].Origin != "wsl-mnt:auzy_" {
		t.Errorf("expected extra home, got %+v", got[0])
	}
}
