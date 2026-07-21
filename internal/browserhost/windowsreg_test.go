package browserhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRegistry is an in-memory RegistryWriter so tests never touch the real
// Windows registry. It records every SetDefault call for assertions.
type fakeRegistry struct {
	values map[string]string
	sets   []string // keyPaths passed to SetDefault, in order
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{values: map[string]string{}}
}

func (f *fakeRegistry) GetDefault(keyPath string) (string, bool, error) {
	v, ok := f.values[keyPath]
	return v, ok, nil
}

func (f *fakeRegistry) SetDefault(keyPath, value string) error {
	f.values[keyPath] = value
	f.sets = append(f.sets, keyPath)
	return nil
}

// mkWinProfile creates a browser's Windows profile dir under windowsHome so
// DetectWindows sees it.
func mkWinProfile(t *testing.T, windowsHome, browserID string) {
	t.Helper()
	for _, b := range browsers {
		if b.ID != browserID {
			continue
		}
		if b.winProfile == "" {
			t.Fatalf("browser %q has no winProfile", browserID)
		}
		dir := filepath.Join(windowsHome, filepath.FromSlash(b.winProfile))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir win profile: %v", err)
		}
		return
	}
	t.Fatalf("unknown browser %q", browserID)
}

// winOpts builds WindowsOptions with a fake registry for tests. The
// WindowsHome is set under a /mnt/<drive> path so the WSL→Windows translation
// works in-test without a real mount.
func winOpts(windowsHome string, reg RegistryWriter) WindowsOptions {
	return WindowsOptions{
		WindowsHome:       windowsHome,
		WSLDistro:         "Ubuntu",
		LinuxLauncherPath: "/home/dev/.observer/browser-host/host-launcher.sh",
		ExtensionIDs:      []string{"abcdefghijklmnopabcdefghijklmnop"},
		Registry:          reg,
	}
}

// fakeToWin is a test path translator: it maps any temp WindowsHome to a
// synthetic C:\ path form so file writes land in the temp dir while the
// manifest/registry still carry a Windows-looking path. It NEVER touches a
// real /mnt/c profile.
func fakeToWin(p string) (string, bool) {
	return `C:\FAKE` + strings.ReplaceAll(p, "/", `\`), true
}

// newTestWindowsRegistrar builds a registrar whose file writes land under a
// temp dir (real WSL path) but whose Windows-form translation is faked, so no
// real /mnt/c/Users profile is ever touched.
func newTestWindowsRegistrar(t *testing.T, opts WindowsOptions) *WindowsRegistrar {
	t.Helper()
	r, err := NewWindowsRegistrar(opts)
	if err != nil {
		t.Fatalf("NewWindowsRegistrar: %v", err)
	}
	r.toWin = fakeToWin
	return r
}

func TestNewWindowsRegistrarValidatesOptions(t *testing.T) {
	fake := newFakeRegistry()
	if _, err := NewWindowsRegistrar(WindowsOptions{WSLDistro: "Ubuntu", LinuxLauncherPath: "/x", Registry: fake}); err == nil {
		t.Error("missing WindowsHome should error")
	}
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", LinuxLauncherPath: "/x", Registry: fake}); err == nil {
		t.Error("missing WSLDistro should error")
	}
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", Registry: fake}); err == nil {
		t.Error("missing LinuxLauncherPath should error")
	}
	// FIX 1: a nil Registry must FAIL LOUDLY — never a silent fallback to the
	// real reg.exe writer (that fallback was the incident's root cause).
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", LinuxLauncherPath: "/x"}); err == nil {
		t.Error("nil Registry must error (no silent fallback to the real machine)")
	}
	// FIX 5: an obviously-invalid extension id is rejected in the constructor.
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", LinuxLauncherPath: "/x", Registry: fake, ExtensionIDs: []string{"NOT-A-VALID-ID"}}); err == nil {
		t.Error("invalid ExtensionID must error in the constructor")
	}
	// A single bad id among otherwise-valid ones still rejects the whole set.
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", LinuxLauncherPath: "/x", Registry: fake, ExtensionIDs: []string{"abcdefghijklmnopabcdefghijklmnop", "NOT-A-VALID-ID"}}); err == nil {
		t.Error("an invalid id among valid ones must error in the constructor")
	}
	// The placeholder sentinel and a real id both pass.
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", LinuxLauncherPath: "/x", Registry: fake, ExtensionIDs: []string{PlaceholderExtensionID}}); err != nil {
		t.Errorf("placeholder ExtensionID should be allowed: %v", err)
	}
	// Multiple valid ids pass.
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", LinuxLauncherPath: "/x", Registry: fake, ExtensionIDs: []string{"abcdefghijklmnopabcdefghijklmnop", "ponmlkjihgfedcbaponmlkjihgfedcba"}}); err != nil {
		t.Errorf("multiple valid ExtensionIDs should be allowed: %v", err)
	}
	if _, err := NewWindowsRegistrar(WindowsOptions{WindowsHome: "/mnt/c/Users/u", WSLDistro: "Ubuntu", LinuxLauncherPath: "/x", Registry: fake}); err != nil {
		t.Errorf("valid options should not error: %v", err)
	}
}

// TestResolveRegExePathFailsClosed pins FIX 3: reg.exe is resolved ONLY from
// the trusted System32 candidates via an INJECTED stat — a fail (nothing
// found) returns an error rather than falling back to a PATH search, and a
// present candidate is selected.
func TestResolveRegExePathFailsClosed(t *testing.T) {
	// Nothing exists anywhere → fail closed with a clear error.
	neverStat := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	if _, err := resolveRegExePath(regExeCandidates(), neverStat); err == nil {
		t.Fatal("resolveRegExePath must fail closed when reg.exe is absent from the trusted path")
	}
	// Only the trusted System32 candidate exists → selected. A PATH-style
	// reg.exe is never even offered as a candidate.
	cands := regExeCandidates()
	want := cands[0]
	onlyTrusted := func(p string) (os.FileInfo, error) {
		if p == want {
			return fakeFileInfo{}, nil
		}
		return nil, os.ErrNotExist
	}
	got, err := resolveRegExePath(cands, onlyTrusted)
	if err != nil {
		t.Fatalf("resolveRegExePath: %v", err)
	}
	if got != want {
		t.Errorf("resolved %q, want the trusted candidate %q", got, want)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c, "/mnt/") || !strings.HasSuffix(c, `/System32/reg.exe`) {
			t.Errorf("candidate %q is not a trusted absolute System32 path", c)
		}
	}
}

// fakeFileInfo is a minimal os.FileInfo for the resolver test.
type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "reg.exe" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

// TestWindowsWriteRejectsSymlinkTarget pins FIX 6: an existing symlink at a
// target artifact path is refused rather than written through.
func TestWindowsWriteRejectsSymlinkTarget(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	r := newTestWindowsRegistrar(t, winOpts(winHome, newFakeRegistry()))

	// Plant a symlink where the bridge would be written.
	dir := r.hostDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir hostDir: %v", err)
	}
	target := filepath.Join(dir, WindowsBridgeName)
	if err := os.Symlink("/etc/passwd", target); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	res := r.Register()
	if res.Error == nil || !strings.Contains(res.Error.Error(), "symlink") {
		t.Fatalf("expected a symlink-refusal error, got %v", res.Error)
	}
	// The symlink target must be untouched (we refused, not followed).
	if got, _ := os.Readlink(target); got != "/etc/passwd" {
		t.Errorf("symlink was modified/followed: readlink = %q", got)
	}
}

// TestWindowsWriteAtomicReplace pins FIX 6's happy path: a re-registration
// with a changed extension id replaces the manifest atomically (final content
// is the new manifest, no leftover temp files in the dir).
func TestWindowsWriteAtomicReplace(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	reg := newFakeRegistry()
	r1 := newTestWindowsRegistrar(t, winOpts(winHome, reg))
	r1.Register()

	opts2 := winOpts(winHome, reg)
	opts2.ExtensionIDs = []string{"ponmlkjihgfedcbaponmlkjihgfedcba"}
	r2 := newTestWindowsRegistrar(t, opts2)
	res := r2.Register()
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	raw, err := os.ReadFile(res.ManifestWSLPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(raw), "ponmlkjihgfedcbaponmlkjihgfedcba") {
		t.Errorf("manifest not replaced with the new id: %s", raw)
	}
	// No leftover temp files from the atomic write.
	entries, _ := os.ReadDir(r2.hostDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".observer-browserhost-") {
			t.Errorf("leftover temp file after atomic replace: %s", e.Name())
		}
	}
}

func TestWslToWindowsPath(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"/mnt/c/Users/tester/.observer/browser-host/x.json", `C:\Users\tester\.observer\browser-host\x.json`, true},
		{"/mnt/d/foo/bar", `D:\foo\bar`, true},
		{"/mnt/c", `C:\`, true},
		{"/home/dev/x", "", false},   // not a /mnt mount
		{"/mnt/cfoo/bar", "", false}, // no slash after drive letter
		{"/mnt/", "", false},
	}
	for _, tc := range tests {
		got, ok := wslToWindowsPath(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("wslToWindowsPath(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestDetectWindowsBrowsers(t *testing.T) {
	home := t.TempDir()
	if got := WindowsBrowsersInstalled(home); len(got) != 0 {
		t.Fatalf("empty home detect = %v, want none", got)
	}
	mkWinProfile(t, home, "chrome")
	mkWinProfile(t, home, "edge")
	got := WindowsBrowsersInstalled(home)
	if len(got) != 2 {
		t.Fatalf("detect = %d, want 2", len(got))
	}
	// Sorted by ID: chrome, edge.
	if got[0].ID != "chrome" || got[1].ID != "edge" {
		t.Errorf("detect order = %v", []string{got[0].ID, got[1].ID})
	}
}

func TestBrowserRegistryKeysPerHive(t *testing.T) {
	// Pins the exact HKCU hives verified against the official docs.
	want := map[string]string{
		"chrome":   `HKCU\Software\Google\Chrome\NativeMessagingHosts\` + HostName,
		"chromium": `HKCU\Software\Chromium\NativeMessagingHosts\` + HostName,
		"edge":     `HKCU\Software\Microsoft\Edge\NativeMessagingHosts\` + HostName,
		"brave":    `HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + HostName,
	}
	for _, b := range browsers {
		got := browserRegistryKey(b)
		if got != want[b.ID] {
			t.Errorf("%s registry key = %q, want %q", b.ID, got, want[b.ID])
		}
		if !strings.HasPrefix(got, `HKCU\`) {
			t.Errorf("%s key must be HKCU-only, got %q", b.ID, got)
		}
	}
}

// TestWindowsRegisterRealTranslationRejectsNonMnt pins the honest production
// failure: with the REAL translator, a WindowsHome not under /mnt/<drive>
// cannot be expressed as a Windows path, so Register errors rather than
// writing a manifest the browser can't use.
func TestWindowsRegisterRealTranslationRejectsNonMnt(t *testing.T) {
	winHome := t.TempDir() // /tmp/... — not a /mnt Windows mount
	mkWinProfile(t, winHome, "chrome")
	r, err := NewWindowsRegistrar(winOpts(winHome, newFakeRegistry()))
	if err != nil {
		t.Fatalf("NewWindowsRegistrar: %v", err)
	}
	res := r.Register() // r.toWin is the real wslToWindowsPath here
	if res.Error == nil || !strings.Contains(res.Error.Error(), "/mnt") {
		t.Fatalf("expected a /mnt path-translation error, got %v", res.Error)
	}
}

func TestWindowsRegisterWritesManifestBridgeAndRegistry(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	mkWinProfile(t, winHome, "brave")

	reg := newFakeRegistry()
	r := newTestWindowsRegistrar(t, winOpts(winHome, reg))
	res := r.Register()
	if res.Error != nil {
		t.Fatalf("Register error: %v", res.Error)
	}
	if !res.FilesWrote {
		t.Errorf("first Register should report FilesWrote")
	}

	// Manifest file: valid JSON, correct path + allowed_origins + type.
	raw, err := os.ReadFile(res.ManifestWSLPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	if m.Name != HostName {
		t.Errorf("manifest name = %q", m.Name)
	}
	if m.Type != "stdio" {
		t.Errorf("manifest type = %q", m.Type)
	}
	if m.Path != res.BridgePath {
		t.Errorf("manifest path = %q, want bridge win path %q", m.Path, res.BridgePath)
	}
	if !strings.HasPrefix(m.Path, `C:\`) {
		t.Errorf("manifest path must be a Windows path, got %q", m.Path)
	}
	wantOrigin := "chrome-extension://abcdefghijklmnopabcdefghijklmnop/"
	if len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != wantOrigin {
		t.Errorf("allowed_origins = %v, want [%s]", m.AllowedOrigins, wantOrigin)
	}

	// Bridge launcher: contains wsl.exe + distro + linux launcher, NO shell
	// interpolation of Chrome's args (no %* / %1 reference).
	bridge, err := os.ReadFile(res.BridgeWSLPath)
	if err != nil {
		t.Fatalf("read bridge: %v", err)
	}
	bs := string(bridge)
	for _, must := range []string{`wsl.exe`, "-d Ubuntu", "/home/dev/.observer/browser-host/host-launcher.sh", "@echo off"} {
		if !strings.Contains(bs, must) {
			t.Errorf("bridge missing %q:\n%s", must, bs)
		}
	}
	if strings.Contains(bs, "%*") || strings.Contains(bs, "%1") {
		t.Errorf("bridge must not forward Chrome's argv (no %%* / %%1):\n%s", bs)
	}
	if strings.Contains(bs, "sh -c") || strings.Contains(bs, "cmd /c") {
		t.Errorf("bridge must not shell-interpolate:\n%s", bs)
	}

	// Registry: one HKCU entry per detected browser (chrome, brave), each
	// pointing at the manifest's Windows path.
	if len(res.Entries) != 2 {
		t.Fatalf("registry entries = %d, want 2", len(res.Entries))
	}
	for _, e := range res.Entries {
		if e.Error != nil {
			t.Errorf("%s registry error: %v", e.Browser, e.Error)
		}
		if !e.Applied {
			t.Errorf("%s should be Applied on first run", e.Browser)
		}
		if !strings.HasPrefix(e.KeyPath, `HKCU\`) {
			t.Errorf("%s key not HKCU: %q", e.Browser, e.KeyPath)
		}
		if got := reg.values[e.KeyPath]; got != res.ManifestPath {
			t.Errorf("%s registry value = %q, want %q", e.Browser, got, res.ManifestPath)
		}
	}
}

func TestWindowsRegisterIdempotent(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	reg := newFakeRegistry()
	r := newTestWindowsRegistrar(t, winOpts(winHome, reg))

	first := r.Register()
	if first.Error != nil || !first.FilesWrote {
		t.Fatalf("first Register = %+v", first)
	}
	if len(reg.sets) != 1 {
		t.Fatalf("first run should SetDefault once, got %d", len(reg.sets))
	}

	// Second run: files byte-identical, registry value already correct.
	r2 := newTestWindowsRegistrar(t, winOpts(winHome, reg))
	second := r2.Register()
	if second.Error != nil {
		t.Fatalf("second Register error: %v", second.Error)
	}
	if !second.FilesAlreadySet {
		t.Errorf("second run should report FilesAlreadySet")
	}
	if second.FilesWrote {
		t.Errorf("second run must not rewrite files")
	}
	if len(reg.sets) != 1 {
		t.Errorf("second run must not call SetDefault again, sets=%d", len(reg.sets))
	}
	for _, e := range second.Entries {
		if !e.AlreadySet {
			t.Errorf("%s should be AlreadySet on re-run", e.Browser)
		}
	}
}

func TestWindowsRegisterDryRunWritesNothing(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	reg := newFakeRegistry()
	opts := winOpts(winHome, reg)
	opts.DryRun = true
	r := newTestWindowsRegistrar(t, opts)
	res := r.Register()
	if res.Error != nil {
		t.Fatalf("dry-run Register error: %v", res.Error)
	}
	if _, err := os.Stat(res.ManifestWSLPath); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create the manifest (stat err = %v)", err)
	}
	if _, err := os.Stat(res.BridgeWSLPath); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create the bridge (stat err = %v)", err)
	}
	if len(reg.sets) != 0 {
		t.Errorf("dry-run must not touch the registry, sets=%d", len(reg.sets))
	}
	// It should still PREVIEW the entries as Applied.
	if len(res.Entries) != 1 || !res.Entries[0].Applied {
		t.Errorf("dry-run should preview one Applied entry, got %+v", res.Entries)
	}
}

func TestWindowsRegisterRewritesOnExtensionIDChange(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	reg := newFakeRegistry()
	r1 := newTestWindowsRegistrar(t, winOpts(winHome, reg))
	r1.Register()

	opts2 := winOpts(winHome, reg)
	opts2.ExtensionIDs = []string{"ponmlkjihgfedcbaponmlkjihgfedcba"}
	r2 := newTestWindowsRegistrar(t, opts2)
	res := r2.Register()
	if res.Error != nil {
		t.Fatalf("Register error: %v", res.Error)
	}
	if !res.FilesWrote {
		t.Errorf("changed extension id must rewrite the manifest")
	}
	raw, _ := os.ReadFile(res.ManifestWSLPath)
	if !strings.Contains(string(raw), "ponmlkjihgfedcbaponmlkjihgfedcba") {
		t.Errorf("manifest not updated with new extension id: %s", raw)
	}
}

func TestWindowsPlaceholderExtensionID(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	opts := winOpts(winHome, newFakeRegistry())
	opts.ExtensionIDs = nil
	r := newTestWindowsRegistrar(t, opts)
	res := r.Register()
	raw, _ := os.ReadFile(res.ManifestWSLPath)
	var m manifest
	_ = json.Unmarshal(raw, &m)
	want := "chrome-extension://" + PlaceholderExtensionID + "/"
	if len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != want {
		t.Errorf("allowed_origins = %v, want [%s]", m.AllowedOrigins, want)
	}
}

// TestWindowsRegisterWritesAllExtensionIDs pins that multiple ids all land in
// the Windows manifest's allowed_origins, in order — the cross-OS case an
// unpacked extension hits (its WSL-Chrome id and its Windows-Chrome id both
// need allowing in the single Windows manifest).
func TestWindowsRegisterWritesAllExtensionIDs(t *testing.T) {
	winHome := t.TempDir()
	mkWinProfile(t, winHome, "chrome")
	opts := winOpts(winHome, newFakeRegistry())
	opts.ExtensionIDs = []string{"abcdefghijklmnopabcdefghijklmnop", "ponmlkjihgfedcbaponmlkjihgfedcba"}
	r := newTestWindowsRegistrar(t, opts)
	res := r.Register()
	if res.Error != nil {
		t.Fatalf("Register error: %v", res.Error)
	}
	raw, _ := os.ReadFile(res.ManifestWSLPath)
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	want := []string{
		"chrome-extension://abcdefghijklmnopabcdefghijklmnop/",
		"chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/",
	}
	if len(m.AllowedOrigins) != len(want) {
		t.Fatalf("allowed_origins = %v, want %v", m.AllowedOrigins, want)
	}
	for i := range want {
		if m.AllowedOrigins[i] != want[i] {
			t.Errorf("allowed_origins[%d] = %q, want %q", i, m.AllowedOrigins[i], want[i])
		}
	}
}

func TestParseRegQueryDefault(t *testing.T) {
	out := "\r\nHKEY_CURRENT_USER\\Software\\Google\\Chrome\\NativeMessagingHosts\\com.superbased.observer.browser\r\n" +
		"    (Default)    REG_SZ    C:\\Users\\tester\\.observer\\browser-host\\com.superbased.observer.browser.json\r\n"
	got, ok := parseRegQueryDefault(out)
	if !ok {
		t.Fatalf("parse failed")
	}
	want := `C:\Users\tester\.observer\browser-host\com.superbased.observer.browser.json`
	if got != want {
		t.Errorf("parsed value = %q, want %q", got, want)
	}
	if _, ok := parseRegQueryDefault("ERROR: The system was unable to find the specified registry key or value.\r\n"); ok {
		t.Errorf("missing key should parse as not-found")
	}
}

func TestBatQuote(t *testing.T) {
	cases := map[string]string{
		"Ubuntu":            "Ubuntu",
		"Ubuntu 22.04":      `"Ubuntu 22.04"`,
		`bad"name`:          "badname",
		`with space"and"qt`: `"with spaceandqt"`,
	}
	for in, want := range cases {
		if got := batQuote(in); got != want {
			t.Errorf("batQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
