//go:build linux

package poll

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

func TestParseEnviron(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"empty", "", nil},
		{"single", "FOO=bar\x00", map[string]string{"FOO": "bar"}},
		{"multi", "A=1\x00B=2\x00", map[string]string{"A": "1", "B": "2"}},
		{"value has equals", "URL=http://x?a=b\x00", map[string]string{"URL": "http://x?a=b"}},
		{"empty value", "EMPTY=\x00", map[string]string{"EMPTY": ""}},
		{"skip no-eq and empty-key", "JUSTNAME\x00=noval\x00OK=1\x00", map[string]string{"OK": "1"}},
		{"no trailing nul", "X=y", map[string]string{"X": "y"}},
		{"dup last wins", "K=1\x00K=2\x00", map[string]string{"K": "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEnviron([]byte(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("parseEnviron(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestDeepEnrichEnvPostureFromChild reads a CHILD process's
// /proc/<pid>/environ — a child, not self, because /proc/<pid>/environ
// reflects the environment as it was at execve and is NOT updated by a
// later setenv/t.Setenv in the same process. The child is given a controlled
// environment, then DeepEnrich reduces it to the posture shape.
func TestDeepEnrichEnvPostureFromChild(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"VIRTUAL_ENV=/home/u/.venvs/myproj",
		"MY_SECRET_TOKEN=supersecret",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8080",
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child process (no /bin/sleep?): %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// /proc/<pid>/environ reflects the environment as of execve; cmd.Start
	// returns after fork but the child's exec of /bin/sleep may not have
	// completed yet. Wait until environ shows the exec'd environment — in
	// production the poll backend only observes a process after a poll tick,
	// so DeepEnrich never races the exec transition.
	environ := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/environ"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(environ); err == nil && strings.Contains(string(raw), "VIRTUAL_ENV=") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	scrub := &processobs.FieldScrubber{
		EnvEnabled:      true,
		StorePathHash:   true,
		MaxPreviewBytes: 512,
		EnvAllowlist:    []string{"MY_SECRET_TOKEN"},
	}
	de := NewDeepEnricher(scrub, false, 0)
	run := &processobs.ProcessRun{PID: cmd.Process.Pid}
	de.DeepEnrich(run)

	if run.EnvPosture == nil {
		t.Fatalf("expected env posture from /proc/%d/environ", cmd.Process.Pid)
	}
	want := map[string]string{
		"VIRTUAL_ENV_present":        "true",
		"VIRTUAL_ENV_basename":       "myproj",
		"ANTHROPIC_BASE_URL_present": "true",
		"MY_SECRET_TOKEN_present":    "true", // secret-looking key → presence only
	}
	for k, v := range want {
		if run.EnvPosture[k] != v {
			t.Errorf("EnvPosture[%q] = %q, want %q (full: %v)", k, run.EnvPosture[k], v, run.EnvPosture)
		}
	}
	if _, ok := run.EnvPosture["PATH_hash"]; !ok {
		t.Errorf("PATH_hash missing: %v", run.EnvPosture)
	}
	// No raw secret value and no raw PATH value may ever appear.
	for k, v := range run.EnvPosture {
		if v == "supersecret" || strings.Contains(v, "/usr/bin") {
			t.Errorf("raw value leaked at %q = %q", k, v)
		}
	}
}

// TestDeepEnrichEnvDisabled confirms a scrubber with EnvEnabled=false (or a
// nil scrubber) reads no environ.
func TestDeepEnrichEnvDisabled(t *testing.T) {
	de := NewDeepEnricher(&processobs.FieldScrubber{EnvEnabled: false}, false, 0)
	run := &processobs.ProcessRun{PID: os.Getpid()}
	de.DeepEnrich(run)
	if run.EnvPosture != nil {
		t.Errorf("env posture should be nil when EnvEnabled=false: %v", run.EnvPosture)
	}

	deNil := NewDeepEnricher(nil, false, 0)
	run2 := &processobs.ProcessRun{PID: os.Getpid()}
	deNil.DeepEnrich(run2)
	if run2.EnvPosture != nil {
		t.Errorf("env posture should be nil with a nil scrubber: %v", run2.EnvPosture)
	}
}

func TestHashExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exe")
	content := []byte("hello world\n")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want := "sha256:" + hex.EncodeToString(sum[:])

	de := &deepEnricher{hashExe: true}
	if got := de.hashExecutable(path); got != want {
		t.Errorf("hashExecutable = %q, want %q", got, want)
	}

	// Over-cap files are skipped (size guard runs before the read).
	deCapped := &deepEnricher{hashExe: true, maxHashBytes: 4}
	if got := deCapped.hashExecutable(path); got != "" {
		t.Errorf("over-cap file should be skipped, got %q", got)
	}

	// A missing/unreadable file hashes to "" (fail-open).
	if got := de.hashExecutable(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("missing file should hash to empty, got %q", got)
	}
}

// TestDeepEnrichExeHashFromSelf hashes the running test binary via
// /proc/self/exe to exercise the real read path. The hash is gated on
// hashExe=true; with it off, ExeHash stays empty (default posture, §19 Q6).
func TestDeepEnrichExeHashFromSelf(t *testing.T) {
	on := NewDeepEnricher(nil, true, 0)
	run := &processobs.ProcessRun{PID: os.Getpid()}
	on.DeepEnrich(run)
	if !strings.HasPrefix(run.ExeHash, "sha256:") {
		t.Errorf("expected an exe hash for self, got %q", run.ExeHash)
	}

	off := NewDeepEnricher(nil, false, 0)
	run2 := &processobs.ProcessRun{PID: os.Getpid()}
	off.DeepEnrich(run2)
	if run2.ExeHash != "" {
		t.Errorf("exe hash should be empty when hashing is off, got %q", run2.ExeHash)
	}
}
