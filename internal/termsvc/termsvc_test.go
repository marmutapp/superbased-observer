package termsvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termrun"
)

// fakeRecorder captures run/correlation writes for assertions.
type fakeRecorder struct {
	mu          sync.Mutex
	runs        []termrun.Run
	ended       map[string]int
	corr        []termrun.Correlation
	recordErr   error
	endCalls    int
	recordCalls int
}

func newFakeRecorder() *fakeRecorder { return &fakeRecorder{ended: map[string]int{}} }

func (f *fakeRecorder) RecordRun(_ context.Context, run termrun.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCalls++
	if f.recordErr != nil {
		return f.recordErr
	}
	f.runs = append(f.runs, run)
	return nil
}

func (f *fakeRecorder) EndRun(_ context.Context, runID string, _ time.Time, code int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endCalls++
	f.ended[runID] = code
	return nil
}

func (f *fakeRecorder) RecordCorrelation(_ context.Context, c termrun.Correlation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.corr = append(f.corr, c)
	return nil
}

// fakeLauncher records the request and returns a fixed handle (or an error).
type fakeLauncher struct {
	handle  string
	err     error
	lastReq LaunchRequest
	calls   int
}

func (f *fakeLauncher) Spawn(req LaunchRequest) (string, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return "", f.err
	}
	return f.handle, nil
}

func newService(t *testing.T, policy Policy, rec RunRecorder, l Launcher, feed *termfeed.Feed) *Service {
	t.Helper()
	return New(Options{
		Policy: policy, Recorder: rec, Launcher: l, Feed: feed,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
}

func TestLaunchFreshAuthorizationGate(t *testing.T) {
	rec := newFakeRecorder()
	l := &fakeLauncher{handle: "H1"}
	tests := []struct {
		name    string
		policy  Policy
		req     FreshRequest
		wantErr error
	}{
		{
			name:    "disabled by default",
			policy:  Policy{},
			req:     FreshRequest{Tool: "claude-code", Subcommand: "claude"},
			wantErr: ErrFreshLaunchDisabled,
		},
		{
			name:    "tool not in allow-list",
			policy:  Policy{AllowFresh: true, AllowedTools: []string{"codex"}},
			req:     FreshRequest{Tool: "claude-code", Subcommand: "claude"},
			wantErr: ErrToolNotAllowed,
		},
		{
			name:    "allowed tool no project root",
			policy:  Policy{AllowFresh: true, AllowedTools: []string{"claude-code"}},
			req:     FreshRequest{Tool: "claude-code", Subcommand: "claude"},
			wantErr: nil,
		},
		{
			name:    "project root denied (no allow-list)",
			policy:  Policy{AllowFresh: true, AllowedTools: []string{"claude-code"}},
			req:     FreshRequest{Tool: "claude-code", Subcommand: "claude", ProjectRoot: t.TempDir()},
			wantErr: ErrProjectRootDenied,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newService(t, tc.policy, rec, l, nil)
			_, err := svc.LaunchFresh(context.Background(), tc.req)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLaunchFreshSuccessMintsRun(t *testing.T) {
	rec := newFakeRecorder()
	feed := termfeed.New(termfeed.Options{})
	sub := feed.Subscribe()
	defer feed.Unsubscribe(sub)
	l := &fakeLauncher{handle: "HANDLE"}
	svc := newService(t, Policy{AllowFresh: true, AllowedTools: []string{"claude-code"}}, rec, l, feed)

	res, err := svc.LaunchFresh(context.Background(), FreshRequest{Tool: "claude-code", Subcommand: "claude", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("LaunchFresh: %v", err)
	}
	if res.Handle != "HANDLE" || res.RunID == "" {
		t.Fatalf("result = %+v", res)
	}
	if len(rec.runs) != 1 {
		t.Fatalf("expected 1 run recorded, got %d", len(rec.runs))
	}
	run := rec.runs[0]
	if run.Kind != termrun.KindFresh || run.Tool != "claude-code" || run.RunID != res.RunID {
		t.Fatalf("run = %+v", run)
	}
	if run.CorrelationTokenHash == "" {
		t.Fatal("expected a correlation token hash on the run")
	}
	// The launcher got a server-derived request with the raw nonce (never
	// persisted) and the fresh kind.
	if l.lastReq.Kind != termrun.KindFresh || l.lastReq.Subcommand != "claude" || l.lastReq.CorrelationToken == "" {
		t.Fatalf("launch request = %+v", l.lastReq)
	}
	if got, ok := svc.RunIDForHandle("HANDLE"); !ok || got != res.RunID {
		t.Fatalf("handle mapping = %q,%v", got, ok)
	}
	// A launch event reached the feed.
	select {
	case ev := <-sub.C():
		if ev.RunID != res.RunID || ev.Trust != termfeed.TrustTrusted {
			t.Fatalf("feed event = %+v", ev)
		}
	default:
		t.Fatal("expected a launch event on the feed")
	}
}

func TestLaunchSpawnFailureClosesRun(t *testing.T) {
	rec := newFakeRecorder()
	l := &fakeLauncher{err: errors.New("spawn boom")}
	svc := newService(t, Policy{AllowFresh: true, AllowedTools: []string{"claude-code"}}, rec, l, nil)
	_, err := svc.LaunchFresh(context.Background(), FreshRequest{Tool: "claude-code", Subcommand: "claude"})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	// The run was recorded then closed out (never left dangling as running).
	if rec.recordCalls != 1 || rec.endCalls != 1 {
		t.Fatalf("record=%d end=%d, want 1/1", rec.recordCalls, rec.endCalls)
	}
}

func TestLaunchHandoffMintsRunWithSource(t *testing.T) {
	rec := newFakeRecorder()
	l := &fakeLauncher{handle: "H"}
	// Handoff does NOT consult the fresh allow-list.
	svc := newService(t, Policy{}, rec, l, nil)
	res, err := svc.LaunchHandoff(context.Background(), HandoffRequest{
		Tool: "codex", Subcommand: "codex", SessionID: "sess-1", Carry: "distilled_tail",
	})
	if err != nil {
		t.Fatalf("LaunchHandoff: %v", err)
	}
	run := rec.runs[0]
	if run.Kind != termrun.KindHandoff || run.SourceSessionID != "sess-1" {
		t.Fatalf("run = %+v", run)
	}
	if run.ProjectRootHash != "" {
		t.Fatalf("handoff run should carry no project-root hash, got %q", run.ProjectRootHash)
	}
	if l.lastReq.SessionID != "sess-1" || l.lastReq.Carry != "distilled_tail" {
		t.Fatalf("launch request = %+v", l.lastReq)
	}
	_ = res
}

func TestEndRunByHandle(t *testing.T) {
	rec := newFakeRecorder()
	l := &fakeLauncher{handle: "HX"}
	svc := newService(t, Policy{AllowFresh: true, AllowedTools: []string{"claude-code"}}, rec, l, nil)
	res, err := svc.LaunchFresh(context.Background(), FreshRequest{Tool: "claude-code", Subcommand: "claude"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	svc.EndRunByHandle(context.Background(), "HX", 7)
	if rec.ended[res.RunID] != 7 {
		t.Fatalf("expected exit 7 recorded, got %v", rec.ended)
	}
	// Handle mapping is forgotten; a second call is a no-op.
	if _, ok := svc.RunIDForHandle("HX"); ok {
		t.Fatal("handle should be forgotten after exit")
	}
	svc.EndRunByHandle(context.Background(), "HX", 9)
	if rec.endCalls != 1 {
		t.Fatalf("expected exactly 1 EndRun, got %d", rec.endCalls)
	}
}

func TestCorrelate(t *testing.T) {
	rec := newFakeRecorder()
	svc := newService(t, Policy{}, rec, &fakeLauncher{}, nil)
	at := time.Unix(1_700_000_100, 0).UTC()
	if err := svc.Correlate(context.Background(), "run-1", "sess-1", termrun.SourceOOB, at); err != nil {
		t.Fatalf("Correlate: %v", err)
	}
	if len(rec.corr) != 1 {
		t.Fatalf("expected 1 correlation, got %d", len(rec.corr))
	}
	c := rec.corr[0]
	if c.RunID != "run-1" || c.SessionID != "sess-1" || c.Source != termrun.SourceOOB || !c.Linkable() {
		t.Fatalf("correlation = %+v", c)
	}
	// Empty ids are a no-op.
	if err := svc.Correlate(context.Background(), "", "s", termrun.SourceOOB, at); err != nil {
		t.Fatalf("Correlate empty: %v", err)
	}
	if len(rec.corr) != 1 {
		t.Fatal("empty correlate should not record")
	}
}
