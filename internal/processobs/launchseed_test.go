package processobs

import (
	"testing"
	"time"
)

// launchSeedTestSession builds a CrossOSSessionRef with sensible defaults.
func launchSeedTestSession(id, tool, root string, started time.Time) CrossOSSessionRef {
	return CrossOSSessionRef{SessionID: id, Tool: tool, ProjectRoot: root, StartedAt: started}
}

func TestMatchLaunchSeeds_BasicMatch(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "opencode", CWD: "/proj", StartedAt: spawn}}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "opencode", "/proj", spawn.Add(30*time.Second)),
	}
	got := MatchLaunchSeeds(seeds, sessions, nil)
	if got[100] != "sess-1" {
		t.Fatalf("MatchLaunchSeeds = %v, want pid 100 → sess-1", got)
	}
}

func TestMatchLaunchSeeds_ToolMismatch(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "opencode", CWD: "/proj", StartedAt: spawn}}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "claude-code", "/proj", spawn.Add(30*time.Second)),
	}
	if got := MatchLaunchSeeds(seeds, sessions, nil); len(got) != 0 {
		t.Fatalf("MatchLaunchSeeds = %v, want no match on tool mismatch", got)
	}
}

func TestMatchLaunchSeeds_CaseInsensitiveTool(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "OpenCode", CWD: "/proj", StartedAt: spawn}}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "opencode", "/proj", spawn.Add(30*time.Second)),
	}
	if got := MatchLaunchSeeds(seeds, sessions, nil); got[100] != "sess-1" {
		t.Fatalf("MatchLaunchSeeds = %v, want case-insensitive tool match", got)
	}
}

func TestMatchLaunchSeeds_CWDMismatch(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "opencode", CWD: "/proj-a", StartedAt: spawn}}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "opencode", "/proj-b", spawn.Add(30*time.Second)),
	}
	if got := MatchLaunchSeeds(seeds, sessions, nil); len(got) != 0 {
		t.Fatalf("MatchLaunchSeeds = %v, want no match on cwd mismatch", got)
	}
}

func TestMatchLaunchSeeds_EmptySeedCWDMatchesAnyRoot(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "opencode", CWD: "", StartedAt: spawn}}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "opencode", "/anywhere", spawn.Add(30*time.Second)),
	}
	if got := MatchLaunchSeeds(seeds, sessions, nil); got[100] != "sess-1" {
		t.Fatalf("MatchLaunchSeeds = %v, want empty seed cwd to match any root", got)
	}
}

func TestMatchLaunchSeeds_WindowBounds(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "opencode", CWD: "/proj", StartedAt: spawn}}

	tooEarly := launchSeedTestSession("sess-early", "opencode", "/proj", spawn.Add(-LaunchSeedBackSkew-time.Second))
	tooLate := launchSeedTestSession("sess-late", "opencode", "/proj", spawn.Add(LaunchSeedForwardWindow+time.Second))
	inWindow := launchSeedTestSession("sess-in", "opencode", "/proj", spawn.Add(LaunchSeedForwardWindow))

	if got := MatchLaunchSeeds(seeds, []CrossOSSessionRef{tooEarly}, nil); len(got) != 0 {
		t.Fatalf("matched session older than back-skew: %v", got)
	}
	if got := MatchLaunchSeeds(seeds, []CrossOSSessionRef{tooLate}, nil); len(got) != 0 {
		t.Fatalf("matched session past forward window: %v", got)
	}
	if got := MatchLaunchSeeds(seeds, []CrossOSSessionRef{inWindow}, nil); got[100] != "sess-in" {
		t.Fatalf("MatchLaunchSeeds = %v, want boundary-inclusive forward match", got)
	}
}

func TestMatchLaunchSeeds_TwoSimultaneousSessionsNotCrossAttributed(t *testing.T) {
	t.Parallel()
	// Two launches in the SAME project seconds apart, two sessions: the
	// injective pairing must give each seed its own session — never both
	// seeds anchoring to one session.
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{
		{PID: 100, Tool: "opencode", CWD: "/proj", StartedAt: spawn},
		{PID: 200, Tool: "opencode", CWD: "/proj", StartedAt: spawn.Add(5 * time.Second)},
	}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-a", "opencode", "/proj", spawn.Add(10*time.Second)),
		launchSeedTestSession("sess-b", "opencode", "/proj", spawn.Add(15*time.Second)),
	}
	got := MatchLaunchSeeds(seeds, sessions, nil)
	if len(got) != 2 {
		t.Fatalf("MatchLaunchSeeds = %v, want two injective pairs", got)
	}
	if got[100] != "sess-a" || got[200] != "sess-b" {
		t.Fatalf("MatchLaunchSeeds = %v, want oldest seed → earliest session pairing", got)
	}
}

func TestMatchLaunchSeeds_ClaimedPIDSkipped(t *testing.T) {
	t.Parallel()
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{{PID: 100, Tool: "opencode", CWD: "/proj", StartedAt: spawn}}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "opencode", "/proj", spawn.Add(30*time.Second)),
	}
	got := MatchLaunchSeeds(seeds, sessions, map[int]bool{100: true})
	if len(got) != 0 {
		t.Fatalf("MatchLaunchSeeds = %v, want claimed pid skipped", got)
	}
}

func TestMatchLaunchSeeds_SingleSessionNotDoubleClaimed(t *testing.T) {
	t.Parallel()
	// One session, two matching seeds (e.g. a relaunch recycled nothing but
	// both pids are pending): only ONE seed may win the session.
	spawn := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seeds := []LaunchSeed{
		{PID: 100, Tool: "opencode", CWD: "/proj", StartedAt: spawn},
		{PID: 200, Tool: "opencode", CWD: "/proj", StartedAt: spawn},
	}
	sessions := []CrossOSSessionRef{
		launchSeedTestSession("sess-1", "opencode", "/proj", spawn.Add(30*time.Second)),
	}
	got := MatchLaunchSeeds(seeds, sessions, nil)
	if len(got) != 1 {
		t.Fatalf("MatchLaunchSeeds = %v, want exactly one claim on a single session", got)
	}
}
