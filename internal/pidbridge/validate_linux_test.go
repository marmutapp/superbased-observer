//go:build linux

package pidbridge

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeProc lays down a synthetic /proc/<pid>/{comm,cmdline} pair under
// dir so validateProcess can be exercised without a real process table.
func writeProc(t *testing.T, dir string, pid int, comm, cmdline string) {
	t.Helper()
	pd := filepath.Join(dir, strconv.Itoa(pid))
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", pd, err)
	}
	if err := os.WriteFile(filepath.Join(pd, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("write comm: %v", err)
	}
	// /proc/<pid>/cmdline is NUL-separated argv.
	if err := os.WriteFile(filepath.Join(pd, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}

func TestValidateProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Alive cline process: comm is the node interpreter (kernel caps
	// comm at 15 chars) but the argv carries the tool path.
	writeProc(t, dir, 4242, "node", "node\x00/usr/lib/node_modules/cline/dist/cli.js\x00")
	// Alive process whose identity does NOT match.
	writeProc(t, dir, 4243, "bash", "bash\x00-lc\x00sleep 100\x00")
	// Alive qwen process whose comm itself matches.
	writeProc(t, dir, 4244, "qwen", "qwen\x00chat\x00")

	cases := []struct {
		name string
		pid  int
		hint string
		want bool
	}{
		{"cline_match_in_cmdline", 4242, "cline", true},
		{"qwen_match_in_comm", 4244, "qwen", true},
		{"identity_mismatch", 4243, "cline", false},
		{"dead_pid", 999999, "cline", false},
		{"pid_le_1", 1, "cline", false},
		{"liveness_only_empty_hint", 4243, "", true},
		{"case_insensitive", 4242, "CLINE", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validateProcess(dir, tc.pid, tc.hint); got != tc.want {
				t.Errorf("validateProcess(%d, %q) = %v; want %v", tc.pid, tc.hint, got, tc.want)
			}
		})
	}
}

// TestValidateLocalProcess_Self confirms the production entrypoint sees
// the running test process as alive (identity-hint drawn from its own
// argv so the check is self-contained).
func TestValidateLocalProcess_Self(t *testing.T) {
	t.Parallel()
	if !ValidateLocalProcess(os.Getpid(), "") {
		t.Error("ValidateLocalProcess(self, liveness-only) = false; want true")
	}
	if ValidateLocalProcess(999999, "cline") {
		t.Error("ValidateLocalProcess(dead pid) = true; want false")
	}
}
