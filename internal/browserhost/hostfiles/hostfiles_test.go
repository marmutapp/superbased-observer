package hostfiles

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteHostWritesBothFilesWithExecBitAndSubstitution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "browser-host")
	launcher, err := WriteHost(dir, Env{
		ObserverBin:    "/opt/observer/bin/observer",
		ObserverConfig: "/home/u/.observer/config.toml",
		NodeBin:        "/usr/bin/node",
	})
	if err != nil {
		t.Fatalf("WriteHost: %v", err)
	}
	if launcher != filepath.Join(dir, LauncherName) {
		t.Errorf("launcher path = %q, want %q", launcher, filepath.Join(dir, LauncherName))
	}
	if launcher != LauncherPath(dir) {
		t.Errorf("WriteHost path %q != LauncherPath %q", launcher, LauncherPath(dir))
	}

	// host.js exists and is the verbatim embedded host (non-empty, has the
	// native-messaging frame reader).
	hostPath := filepath.Join(dir, HostScriptName)
	host, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read host.js: %v", err)
	}
	if !strings.Contains(string(host), "native-messaging") {
		t.Errorf("host.js missing expected content")
	}

	// Launcher: OBSERVER_BIN / OBSERVER_CONFIG / node substituted; no markers
	// left over.
	raw, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		`export OBSERVER_BIN="/opt/observer/bin/observer"`,
		`export OBSERVER_CONFIG="/home/u/.observer/config.toml"`,
		`exec "/usr/bin/node"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("launcher missing %q\n---\n%s", want, s)
		}
	}
	for _, marker := range []string{observerBinMarker, observerConfigMarker, nodeBinMarker} {
		if strings.Contains(s, marker) {
			t.Errorf("launcher still contains unsubstituted marker %q", marker)
		}
	}

	// Exec bit on the launcher, not on host.js (POSIX only — the runner box).
	if runtime.GOOS != "windows" {
		lfi, _ := os.Stat(launcher)
		if lfi.Mode().Perm()&0o111 == 0 {
			t.Errorf("launcher mode = %v, want executable", lfi.Mode().Perm())
		}
		hfi, _ := os.Stat(hostPath)
		if hfi.Mode().Perm() != 0o644 {
			t.Errorf("host.js mode = %v, want 0644", hfi.Mode().Perm())
		}
	}
}

func TestWriteHostDefaultsWhenEnvEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteHost(dir, Env{}); err != nil {
		t.Fatalf("WriteHost: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, LauncherName))
	s := string(raw)
	if !strings.Contains(s, `export OBSERVER_BIN="observer"`) {
		t.Errorf("empty ObserverBin should default to \"observer\": %s", s)
	}
	if !strings.Contains(s, `export OBSERVER_CONFIG=""`) {
		t.Errorf("empty ObserverConfig should render empty string: %s", s)
	}
	if !strings.Contains(s, `exec "node"`) {
		t.Errorf("empty NodeBin should default to \"node\": %s", s)
	}
}

func TestWriteHostIdempotentRewrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteHost(dir, Env{ObserverBin: "/a"}); err != nil {
		t.Fatalf("first WriteHost: %v", err)
	}
	if _, err := WriteHost(dir, Env{ObserverBin: "/b"}); err != nil {
		t.Fatalf("second WriteHost: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, LauncherName))
	if !strings.Contains(string(raw), `export OBSERVER_BIN="/b"`) {
		t.Errorf("re-write did not update OBSERVER_BIN")
	}
}

func TestShellEscapeDoubleQuoted(t *testing.T) {
	tests := []struct{ in, want string }{
		{`/plain/path`, `/plain/path`},
		{`/has space/observer`, `/has space/observer`},
		{`/has"quote`, `/has\"quote`},
		{`/has$var`, `/has\$var`},
		{"/has`tick", "/has\\`tick"},
		{`/back\slash`, `/back\\slash`},
	}
	for _, tc := range tests {
		if got := shellEscapeDoubleQuoted(tc.in); got != tc.want {
			t.Errorf("shellEscapeDoubleQuoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFilesDeterministic(t *testing.T) {
	f := Files()
	if len(f) != 2 || f[0] != HostScriptName || f[1] != LauncherName {
		t.Errorf("Files() = %v, want [%s %s]", f, HostScriptName, LauncherName)
	}
}
