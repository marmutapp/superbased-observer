package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSelectTerminalRunRows_FieldsAndCorrelation(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sessionID, _ := mustProjectAndSession(t, s)
	now := time.Now().UTC()

	if err := s.InsertTerminalRun(ctx, TerminalRun{
		RunID:      "run-attach-1",
		Tool:       "claude-code",
		Kind:       "attach",
		LaunchedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("InsertTerminalRun: %v", err)
	}
	ended := now
	exitCode := 0
	if err := s.EndTerminalRun(ctx, "run-attach-1", ended, exitCode, EndReasonChildExit); err != nil {
		t.Fatalf("EndTerminalRun: %v", err)
	}
	if err := s.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID:      "run-attach-1",
		SessionID:  sessionID,
		Confidence: 0.9,
		Source:     "oob",
	}); err != nil {
		t.Fatalf("UpsertCorrelation: %v", err)
	}
	// A weaker correlation must not win over the stronger one above.
	if err := s.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID:      "run-attach-1",
		SessionID:  "some-other-session",
		Confidence: 0.2,
		Source:     "heuristic",
	}); err != nil {
		t.Fatalf("UpsertCorrelation (weak): %v", err)
	}
	if err := s.InsertTerminalCommand(ctx, TerminalCommand{
		RunID:     "run-attach-1",
		TurnSeq:   1,
		StartedAt: now.Add(-30 * time.Second),
		Trust:     "oob",
		CmdHash:   "hash-1",
	}); err != nil {
		t.Fatalf("InsertTerminalCommand: %v", err)
	}
	if err := s.InsertTerminalCommand(ctx, TerminalCommand{
		RunID:     "run-attach-1",
		TurnSeq:   2,
		StartedAt: now.Add(-10 * time.Second),
		Trust:     "hint",
		CmdHash:   "hash-2",
	}); err != nil {
		t.Fatalf("InsertTerminalCommand: %v", err)
	}

	got, err := s.SelectTerminalRunRows(ctx)
	if err != nil {
		t.Fatalf("SelectTerminalRunRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	r := got[0]
	if r.RunID != "run-attach-1" || r.Tool != "claude-code" || r.Kind != "attach" {
		t.Errorf("run identity = %+v", r)
	}
	if r.CorrelatedSessionID != sessionID {
		t.Errorf("CorrelatedSessionID = %q, want the strongest match %q", r.CorrelatedSessionID, sessionID)
	}
	if r.CorrelatedConfidence != 0.9 {
		t.Errorf("CorrelatedConfidence = %v, want 0.9", r.CorrelatedConfidence)
	}
	if !r.Exited || r.EndedAt == "" {
		t.Errorf("Exited/EndedAt = %v/%q, want exited", r.Exited, r.EndedAt)
	}
	if r.EndReason != EndReasonChildExit {
		t.Errorf("EndReason = %q, want %q", r.EndReason, EndReasonChildExit)
	}
	if r.CommandCount != 2 {
		t.Errorf("CommandCount = %d, want 2", r.CommandCount)
	}
	// No raw command text or output field exists on the row type at all — this
	// is a compile-time property (orgcontract.TerminalRunRow / TerminalCommandRow
	// have no such field), not something a runtime assertion can check further.
}

func TestSelectTerminalRunRows_ExcludesOldRuns(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.InsertTerminalRun(ctx, TerminalRun{
		RunID:      "run-old",
		Tool:       "codex",
		Kind:       "fresh",
		LaunchedAt: now.AddDate(0, 0, -30),
	}); err != nil {
		t.Fatalf("InsertTerminalRun(old): %v", err)
	}
	if err := s.InsertTerminalRun(ctx, TerminalRun{
		RunID:      "run-current",
		Tool:       "codex",
		Kind:       "fresh",
		LaunchedAt: now,
	}); err != nil {
		t.Fatalf("InsertTerminalRun(current): %v", err)
	}

	got, err := s.SelectTerminalRunRows(ctx)
	if err != nil {
		t.Fatalf("SelectTerminalRunRows: %v", err)
	}
	if len(got) != 1 || got[0].RunID != "run-current" {
		t.Fatalf("got = %+v, want exactly [run-current]", got)
	}
}

func TestSelectTerminalRunRows_NoRuns(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	got, err := s.SelectTerminalRunRows(context.Background())
	if err != nil {
		t.Fatalf("SelectTerminalRunRows: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got = %v, want empty non-nil slice", got)
	}
}

func TestSelectTerminalCommandRows_CapsPerRunMostRecent(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.InsertTerminalRun(ctx, TerminalRun{
		RunID:      "run-busy",
		Tool:       "claude-code",
		Kind:       "fresh",
		LaunchedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("InsertTerminalRun: %v", err)
	}
	total := terminalCommandsPerRunCap + 5
	for i := 0; i < total; i++ {
		if err := s.InsertTerminalCommand(ctx, TerminalCommand{
			RunID:     "run-busy",
			TurnSeq:   i,
			StartedAt: now.Add(time.Duration(i) * time.Second),
			Trust:     "oob",
			CmdHash:   fmt.Sprintf("hash-%d", i),
		}); err != nil {
			t.Fatalf("InsertTerminalCommand[%d]: %v", i, err)
		}
	}

	got, err := s.SelectTerminalCommandRows(ctx)
	if err != nil {
		t.Fatalf("SelectTerminalCommandRows: %v", err)
	}
	if len(got) != terminalCommandsPerRunCap {
		t.Fatalf("len(got) = %d, want %d (capped)", len(got), terminalCommandsPerRunCap)
	}
	// The kept rows must be the most-recent turn_seq values, i.e. the top
	// terminalCommandsPerRunCap out of [0, total).
	minKeptSeq := total - terminalCommandsPerRunCap
	for _, c := range got {
		if int(c.TurnSeq) < minKeptSeq {
			t.Errorf("kept turn_seq %d < min expected %d — cap dropped a recent row instead of an old one", c.TurnSeq, minKeptSeq)
		}
		if c.RunID != "run-busy" {
			t.Errorf("RunID = %q, want run-busy", c.RunID)
		}
	}
}

func TestSelectTerminalCommandRows_NoRawCommandData(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.InsertTerminalRun(ctx, TerminalRun{
		RunID:      "run-hash-only",
		Tool:       "claude-code",
		Kind:       "fresh",
		LaunchedAt: now,
	}); err != nil {
		t.Fatalf("InsertTerminalRun: %v", err)
	}
	if err := s.InsertTerminalCommand(ctx, TerminalCommand{
		RunID:     "run-hash-only",
		TurnSeq:   1,
		StartedAt: now,
		Trust:     "oob",
		CmdHash:   "hash-only",
	}); err != nil {
		t.Fatalf("InsertTerminalCommand: %v", err)
	}
	got, err := s.SelectTerminalCommandRows(ctx)
	if err != nil {
		t.Fatalf("SelectTerminalCommandRows: %v", err)
	}
	if len(got) != 1 || got[0].CmdHash != "hash-only" || got[0].Trust != "oob" {
		t.Fatalf("got = %+v", got)
	}
}

func TestSelectRemoteAuditRows_FieldsAndCap(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.InsertRemoteAudit(ctx, RemoteAuditEvent{
		TS:         now,
		Kind:       "session_paired",
		SessionID:  "sess-remote-1",
		Principal:  "execute",
		RemoteAddr: "203.0.113.7:52344",
		Route:      "/api/remote/pair",
		Decision:   "allow",
		Detail:     "paired via QR",
	}); err != nil {
		t.Fatalf("InsertRemoteAudit: %v", err)
	}

	got, err := s.SelectRemoteAuditRows(ctx)
	if err != nil {
		t.Fatalf("SelectRemoteAuditRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	r := got[0]
	if r.EventKey == "" {
		t.Errorf("EventKey is empty, want the stringified row id")
	}
	if r.Kind != "session_paired" || r.SessionID != "sess-remote-1" || r.Principal != "execute" {
		t.Errorf("identity fields = %+v", r)
	}
	// RemoteAddr ships RAW per org-parity plan §0.1 ("peer addresses INCLUDED").
	if r.RemoteAddr != "203.0.113.7:52344" {
		t.Errorf("RemoteAddr = %q, want the raw peer address unmodified", r.RemoteAddr)
	}
	if r.Decision != "allow" || r.Detail != "paired via QR" {
		t.Errorf("decision/detail = %+v", r)
	}
}

func TestSelectRemoteAuditRows_CapAndExcludesOld(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.InsertRemoteAudit(ctx, RemoteAuditEvent{
		TS:   now.AddDate(0, 0, -30),
		Kind: "session_paired",
	}); err != nil {
		t.Fatalf("InsertRemoteAudit(old): %v", err)
	}
	total := remoteAuditEventCap + 5
	for i := 0; i < total; i++ {
		if err := s.InsertRemoteAudit(ctx, RemoteAuditEvent{
			TS:   now.Add(time.Duration(i) * time.Millisecond),
			Kind: "http_request",
		}); err != nil {
			t.Fatalf("InsertRemoteAudit[%d]: %v", i, err)
		}
	}

	got, err := s.SelectRemoteAuditRows(ctx)
	if err != nil {
		t.Fatalf("SelectRemoteAuditRows: %v", err)
	}
	if len(got) != remoteAuditEventCap {
		t.Fatalf("len(got) = %d, want %d (capped, old row excluded)", len(got), remoteAuditEventCap)
	}
	for _, r := range got {
		if r.Kind != "http_request" {
			t.Errorf("Kind = %q, want http_request (the old session_paired row must be excluded)", r.Kind)
		}
	}
}

func TestSelectRemoteAuditRows_NoEvents(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	got, err := s.SelectRemoteAuditRows(context.Background())
	if err != nil {
		t.Fatalf("SelectRemoteAuditRows: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got = %v, want empty non-nil slice", got)
	}
}
