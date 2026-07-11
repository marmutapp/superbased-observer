package browserhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mkProfile creates the browser's user-data dir under home so Detect sees it.
func mkProfile(t *testing.T, home, goos, browserID string) {
	t.Helper()
	for _, b := range browsers {
		if b.ID != browserID {
			continue
		}
		dir, ok := b.profileDir(home, goos)
		if !ok {
			t.Fatalf("no dir for %s on %s", browserID, goos)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir profile: %v", err)
		}
		return
	}
	t.Fatalf("unknown browser %q", browserID)
}

func TestDetectSkipsMissingProfileDirs(t *testing.T) {
	home := t.TempDir()
	r, err := NewRegistrar(Options{Home: home, GOOS: "linux"})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	if got := r.Detect(); len(got) != 0 {
		t.Fatalf("Detect on empty home = %v, want none", got)
	}
	// Create chrome + brave profile dirs; edge/chromium stay absent.
	mkProfile(t, home, "linux", "chrome")
	mkProfile(t, home, "linux", "brave")
	got := r.Detect()
	if len(got) != 2 {
		t.Fatalf("Detect = %d browsers, want 2", len(got))
	}
	// Deterministic sort by ID: brave, chrome.
	if got[0].ID != "brave" || got[1].ID != "chrome" {
		t.Errorf("Detect order = %v, want [brave chrome]", []string{got[0].ID, got[1].ID})
	}
}

func TestRegisterWritesManifestPerBrowser(t *testing.T) {
	home := t.TempDir()
	mkProfile(t, home, "linux", "chrome")
	mkProfile(t, home, "linux", "edge")
	r, err := NewRegistrar(Options{
		Home: home, GOOS: "linux",
		HostPath:    "/opt/observer/browser-host/host-launcher",
		ExtensionID: "abcdefghijklmnopabcdefghijklmnop",
	})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	results := r.Register()
	if len(results) != 2 {
		t.Fatalf("Register = %d results, want 2", len(results))
	}
	for _, res := range results {
		if res.Error != nil {
			t.Fatalf("%s: %v", res.Browser, res.Error)
		}
		if !res.Wrote {
			t.Errorf("%s: Wrote=false, want true", res.Browser)
		}
		// The manifest must live under the browser's own NativeMessagingHosts.
		if filepath.Base(res.ConfigPath) != manifestFileName {
			t.Errorf("%s: ConfigPath = %q, want basename %q", res.Browser, res.ConfigPath, manifestFileName)
		}
		if filepath.Base(filepath.Dir(res.ConfigPath)) != "NativeMessagingHosts" {
			t.Errorf("%s: manifest not under NativeMessagingHosts: %q", res.Browser, res.ConfigPath)
		}
		raw, err := os.ReadFile(res.ConfigPath)
		if err != nil {
			t.Fatalf("read %s: %v", res.ConfigPath, err)
		}
		var m manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("manifest not valid JSON: %v", err)
		}
		if m.Name != HostName {
			t.Errorf("manifest name = %q, want %q", m.Name, HostName)
		}
		if m.Path != "/opt/observer/browser-host/host-launcher" {
			t.Errorf("manifest path = %q", m.Path)
		}
		if len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != "chrome-extension://abcdefghijklmnopabcdefghijklmnop/" {
			t.Errorf("manifest allowed_origins = %v", m.AllowedOrigins)
		}
		if m.Type != "stdio" {
			t.Errorf("manifest type = %q, want stdio", m.Type)
		}
	}
}

func TestRegisterIdempotent(t *testing.T) {
	home := t.TempDir()
	mkProfile(t, home, "linux", "chrome")
	opts := Options{Home: home, GOOS: "linux", HostPath: "/x/host", ExtensionID: "id"}
	r, _ := NewRegistrar(opts)

	first := r.Register()
	if len(first) != 1 || !first[0].Wrote {
		t.Fatalf("first Register = %+v, want one Wrote", first)
	}
	second := r.Register()
	if len(second) != 1 || !second[0].AlreadySet {
		t.Fatalf("second Register = %+v, want AlreadySet (idempotent)", second)
	}
	if second[0].Wrote {
		t.Errorf("idempotent re-run must not report Wrote")
	}
}

func TestRegisterRewritesOnChange(t *testing.T) {
	home := t.TempDir()
	mkProfile(t, home, "linux", "chrome")
	r1, _ := NewRegistrar(Options{Home: home, GOOS: "linux", HostPath: "/a", ExtensionID: "id1"})
	r1.Register()
	// A different extension id must produce a fresh write, not AlreadySet.
	r2, _ := NewRegistrar(Options{Home: home, GOOS: "linux", HostPath: "/a", ExtensionID: "id2"})
	res := r2.Register()
	if len(res) != 1 || !res[0].Wrote {
		t.Fatalf("changed manifest Register = %+v, want Wrote", res)
	}
}

func TestRegisterDryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	mkProfile(t, home, "linux", "chrome")
	r, _ := NewRegistrar(Options{Home: home, GOOS: "linux", HostPath: "/x", ExtensionID: "id", DryRun: true})
	res := r.Register()
	if len(res) != 1 || !res[0].Wrote {
		t.Fatalf("dry-run Register = %+v, want Wrote (preview)", res)
	}
	if _, err := os.Stat(res[0].ConfigPath); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create the manifest file (stat err = %v)", err)
	}
}

func TestPlaceholderExtensionIDWhenAbsent(t *testing.T) {
	home := t.TempDir()
	mkProfile(t, home, "linux", "chrome")
	r, _ := NewRegistrar(Options{Home: home, GOOS: "linux", HostPath: "/x"})
	res := r.Register()
	raw, _ := os.ReadFile(res[0].ConfigPath)
	var m manifest
	_ = json.Unmarshal(raw, &m)
	want := "chrome-extension://" + PlaceholderExtensionID + "/"
	if m.AllowedOrigins[0] != want {
		t.Errorf("allowed_origins[0] = %q, want %q", m.AllowedOrigins[0], want)
	}
}

func TestPerBrowserDirTable(t *testing.T) {
	// Pins the per-browser × per-OS lookup table: each browser resolves a
	// distinct NativeMessagingHosts dir on linux + darwin.
	tests := []struct {
		id, goos, wantContains string
	}{
		{"chrome", "linux", ".config/google-chrome/NativeMessagingHosts"},
		{"chromium", "linux", ".config/chromium/NativeMessagingHosts"},
		{"edge", "linux", ".config/microsoft-edge/NativeMessagingHosts"},
		{"brave", "linux", ".config/BraveSoftware/Brave-Browser/NativeMessagingHosts"},
		{"chrome", "darwin", "Library/Application Support/Google/Chrome/NativeMessagingHosts"},
		{"edge", "darwin", "Library/Application Support/Microsoft Edge/NativeMessagingHosts"},
	}
	home := "/home/u"
	for _, tc := range tests {
		var b Browser
		for _, cand := range browsers {
			if cand.ID == tc.id {
				b = cand
			}
		}
		got, ok := b.nativeMessagingDir(home, tc.goos)
		if !ok {
			t.Errorf("%s/%s: no dir", tc.id, tc.goos)
			continue
		}
		want := filepath.Join(home, filepath.FromSlash(tc.wantContains))
		if got != want {
			t.Errorf("%s/%s dir = %q, want %q", tc.id, tc.goos, got, want)
		}
	}
}

func TestInstalledReportsDetectedIDs(t *testing.T) {
	home := t.TempDir()
	mkProfile(t, home, "linux", "chrome")
	mkProfile(t, home, "linux", "chromium")
	r, _ := NewRegistrar(Options{Home: home, GOOS: "linux"})
	got := r.Installed()
	if len(got) != 2 {
		t.Fatalf("Installed = %v, want 2", got)
	}
	// Sorted by ID: chrome, chromium.
	if got[0] != "chrome" || got[1] != "chromium" {
		t.Errorf("Installed = %v, want [chrome chromium]", got)
	}
}

func TestNewRegistrarDefaultsToRealHomeAndGOOS(t *testing.T) {
	// No Home/GOOS override → resolves os.UserHomeDir + runtime.GOOS without
	// erroring (the default-path branch). Detect may be empty on CI; we only
	// assert construction succeeds.
	r, err := NewRegistrar(Options{})
	if err != nil {
		t.Fatalf("NewRegistrar default: %v", err)
	}
	_ = r.Detect()
}

func TestDetectIgnoresAFileNamedLikeAProfileDir(t *testing.T) {
	home := t.TempDir()
	// Create a FILE where chrome's profile dir would be — Detect must not
	// treat it as a browser (it stats for IsDir()).
	chromeParent := filepath.Join(home, ".config")
	if err := os.MkdirAll(chromeParent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chromeParent, "google-chrome"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, _ := NewRegistrar(Options{Home: home, GOOS: "linux"})
	if got := r.Detect(); len(got) != 0 {
		t.Errorf("Detect = %v, want none (a file is not a profile dir)", got)
	}
}

func TestWindowsHasNoGroundedDir(t *testing.T) {
	// Windows is registry-based, not dir-based — the table omits it, so
	// Register honestly writes nothing rather than a file the browser
	// never reads.
	home := t.TempDir()
	// Even if a chrome profile dir "existed", windows has no map entry.
	r, _ := NewRegistrar(Options{Home: home, GOOS: "windows"})
	if got := r.Detect(); len(got) != 0 {
		t.Errorf("Detect on windows = %v, want none (registry-based, not wired)", got)
	}
}
