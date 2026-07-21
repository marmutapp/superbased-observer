package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestWorkspaceLayoutRoundTrip pins the workspace_layouts seam (migration 073):
// unsaved → ok=false; save → load round-trips byte-identically; a second save
// upserts (overwrites, no duplicate row); invalid JSON and oversize blobs are
// rejected with ErrWorkspaceLayoutInvalid; the empty name is refused.
func TestWorkspaceLayoutRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.GetWorkspaceLayout(ctx, "default"); err != nil || ok {
		t.Fatalf("unsaved workspace: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	l1 := `{"lg":[{"i":"h1","x":0,"y":0,"w":6,"h":10}],"tray":[]}`
	if err := s.SaveWorkspaceLayout(ctx, "default", l1); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	got, ok, err := s.GetWorkspaceLayout(ctx, "default")
	if err != nil || !ok || got != l1 {
		t.Fatalf("load 1: got=%q ok=%v err=%v, want the saved blob", got, ok, err)
	}

	l2 := `{"lg":[],"tray":["h1"]}`
	if err := s.SaveWorkspaceLayout(ctx, "default", l2); err != nil {
		t.Fatalf("save 2 (upsert): %v", err)
	}
	if got, _, _ := s.GetWorkspaceLayout(ctx, "default"); got != l2 {
		t.Fatalf("upsert did not overwrite: got %q", got)
	}

	if err := s.SaveWorkspaceLayout(ctx, "default", `{not json`); !errors.Is(err, ErrWorkspaceLayoutInvalid) {
		t.Fatalf("invalid JSON accepted: err=%v", err)
	}
	big := `{"pad":"` + strings.Repeat("x", MaxWorkspaceLayoutBytes) + `"}`
	if err := s.SaveWorkspaceLayout(ctx, "default", big); !errors.Is(err, ErrWorkspaceLayoutInvalid) {
		t.Fatalf("oversize blob accepted: err=%v", err)
	}
	if err := s.SaveWorkspaceLayout(ctx, "", l1); err == nil {
		t.Fatal("empty workspace name accepted")
	}
	// The rejected saves must not have clobbered the stored layout.
	if got, _, _ := s.GetWorkspaceLayout(ctx, "default"); got != l2 {
		t.Fatalf("rejected saves mutated the stored layout: got %q", got)
	}
}
