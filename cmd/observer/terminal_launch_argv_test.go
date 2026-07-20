package main

import (
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termrun"
	"github.com/marmutapp/superbased-observer/internal/termsession"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// recordingSpawner captures the last Spec the Manager builds so the test can
// assert the argv SHAPE ptyLauncher constructed for a given run kind.
type recordingSpawner struct {
	mu   sync.Mutex
	last termsession.Spec
}

func (s *recordingSpawner) Spawn(spec termsession.Spec) (termsession.PTY, error) {
	s.mu.Lock()
	s.last = spec
	s.mu.Unlock()
	return newFakePTY(), nil
}

func (s *recordingSpawner) lastSpec() termsession.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// TestPtyLauncherArgvModePerKind pins the F2 seam: ptyLauncher.Spawn maps each
// run KIND onto the correct argv SHAPE through the captured Spec (ArgvMode +
// ExtraArgs), so the inner `observer <sub>` launch is never a self-conflicting
// argv. Crucially a KindResume launch is a FRESH-style base with its
// `--resume <id>` tail in ExtraArgs — NOT a handoff `--continue-from … --resume
// …` which claude/codex reject (the pre-fix bug). This asserts the Spec fields
// (the seam ptyLauncher owns); the EXACT argv each ArgvMode yields is pinned in
// internal/termsession TestSpecArgvModes.
func TestPtyLauncherArgvModePerKind(t *testing.T) {
	cases := []struct {
		name         string
		req          termsvc.LaunchRequest
		wantMode     termsession.ArgvMode
		wantExtra    []string
		wantSessID   string
		wantContinue bool // whether the argv this Spec yields carries --continue-from
	}{
		{
			name:     "fresh",
			req:      termsvc.LaunchRequest{Kind: termrun.KindFresh, Subcommand: "claude"},
			wantMode: termsession.ArgvModeFresh,
		},
		{
			name:      "attach with escape hatch",
			req:       termsvc.LaunchRequest{Kind: termrun.KindAttach, Subcommand: "claude", ExtraArgs: []string{"--no-proxy-route"}},
			wantMode:  termsession.ArgvModeFresh,
			wantExtra: []string{"--no-proxy-route"},
		},
		{
			name:       "resume is fresh base + resume tail (NOT continue-from)",
			req:        termsvc.LaunchRequest{Kind: termrun.KindResume, Subcommand: "claude", SessionID: "sess-1", ExtraArgs: []string{"--resume", "sess-1"}},
			wantMode:   termsession.ArgvModeFresh,
			wantExtra:  []string{"--resume", "sess-1"},
			wantSessID: "sess-1",
		},
		{
			name:         "handoff takes continue-from",
			req:          termsvc.LaunchRequest{Kind: termrun.KindHandoff, Subcommand: "codex", SessionID: "src-1", Carry: "full"},
			wantMode:     termsession.ArgvModeHandoff,
			wantSessID:   "src-1",
			wantContinue: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &recordingSpawner{}
			mgr := termsession.NewManager(termsession.Options{
				Spawner: sp, ReapInterval: time.Hour, Now: time.Now,
			})
			t.Cleanup(mgr.Shutdown)
			launcher := &ptyLauncher{mgr: mgr, binPath: "/observer"}
			if _, err := launcher.Spawn(tc.req); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			spec := sp.lastSpec()
			if spec.ArgvMode != tc.wantMode {
				t.Fatalf("ArgvMode = %d, want %d", spec.ArgvMode, tc.wantMode)
			}
			if !equalArgs(spec.ExtraArgs, tc.wantExtra) {
				t.Fatalf("ExtraArgs = %v, want %v", spec.ExtraArgs, tc.wantExtra)
			}
			if spec.SessionID != tc.wantSessID {
				t.Fatalf("SessionID = %q, want %q", spec.SessionID, tc.wantSessID)
			}
			// A resume's SessionID is carried for identity but its ArgvModeFresh
			// shape yields NO --continue-from — the whole point of F2. Only a
			// handoff produces the --continue-from argv.
			gotContinue := spec.ArgvMode == termsession.ArgvModeHandoff
			if gotContinue != tc.wantContinue {
				t.Fatalf("continue-from argv = %v, want %v", gotContinue, tc.wantContinue)
			}
		})
	}
}
