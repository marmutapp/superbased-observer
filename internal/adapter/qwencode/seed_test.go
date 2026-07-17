package qwencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// writeRuntimeSidecar drops a realistic <uuid>.runtime.json next to a
// transcript path. Shape mirrors testdata/qwencode/session.runtime.json
// (pid / hostname / work_dir / qwen_version / session_id / started_at).
func writeRuntimeSidecar(t *testing.T, transcriptPath string, pid int, hostname, sessionID, workDir string) {
	t.Helper()
	base := transcriptPath[:len(transcriptPath)-len(".jsonl")]
	body, _ := json.Marshal(map[string]any{
		"hostname":       hostname,
		"pid":            pid,
		"qwen_version":   "0.19.8",
		"schema_version": 1,
		"session_id":     sessionID,
		"started_at":     1780701711,
		"work_dir":       workDir,
	})
	if err := os.WriteFile(base+".runtime.json", body, 0o600); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}
}

func TestRuntimeSeeds(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("no hostname available")
	}
	dir := t.TempDir()
	transcript := filepath.Join(dir, "aaaa-bbbb.jsonl")

	// Local host → seed emitted.
	writeRuntimeSidecar(t, transcript, 4242, host, "sess-local", "/home/dev/proj")
	seeds := runtimeSeeds(transcript)
	if len(seeds) != 1 {
		t.Fatalf("len(seeds) = %d; want 1 for a local-host sidecar", len(seeds))
	}
	want := models.SessionProcessSeed{PID: 4242, SessionID: "sess-local", Tool: models.ToolQwenCode, CWD: "/home/dev/proj", ExecHint: "qwen"}
	if seeds[0] != want {
		t.Errorf("seed = %+v; want %+v", seeds[0], want)
	}

	// Foreign host → no seed (pid lives on another machine).
	writeRuntimeSidecar(t, transcript, 4242, host+"-other", "sess-foreign", "/home/dev/proj")
	if s := runtimeSeeds(transcript); s != nil {
		t.Errorf("foreign-host seeds = %+v; want nil", s)
	}

	// Missing sidecar → no seed.
	if s := runtimeSeeds(filepath.Join(dir, "no-sidecar.jsonl")); s != nil {
		t.Errorf("missing-sidecar seeds = %+v; want nil", s)
	}

	// pid <= 1 → no seed.
	writeRuntimeSidecar(t, transcript, 1, host, "sess-badpid", "/home/dev/proj")
	if s := runtimeSeeds(transcript); s != nil {
		t.Errorf("bad-pid seeds = %+v; want nil", s)
	}
}

// TestRuntimeSidecarNotASessionFile pins that adding the targeted
// sidecar read did NOT turn the sidecar into a dispatched session file —
// the watcher must still ignore <uuid>.runtime.json.
func TestRuntimeSidecarNotASessionFile(t *testing.T) {
	t.Parallel()
	a := NewWithOptions(nil, "/home/dev/.qwen/projects")
	transcript := "/home/dev/.qwen/projects/slug/chats/aaaa.jsonl"
	sidecar := "/home/dev/.qwen/projects/slug/chats/aaaa.runtime.json"
	if !a.IsSessionFile(transcript) {
		t.Errorf("transcript %q should be a session file", transcript)
	}
	if a.IsSessionFile(sidecar) {
		t.Errorf("runtime.json sidecar %q must NOT be treated as a session file", sidecar)
	}
}
