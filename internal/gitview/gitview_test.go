package gitview

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseStatusPorcelainV2(t *testing.T) {
	// -z output: NUL-terminated records. Rename "2" records are followed by a
	// NUL-separated origin-path token.
	fields := []string{
		"# branch.oid abcdef",
		"# branch.head main",
		"# branch.upstream origin/main",
		"# branch.ab +2 -1",
		"1 M. N... 100644 100644 100644 1111111 2222222 staged.go",
		"1 .M N... 100644 100644 100644 3333333 4444444 dirty.go",
		"2 R. N... 100644 100644 100644 5555555 6666666 R100 new.go",
		"old.go", // origin path for the rename above
		"? untracked.txt",
		"! ignored.txt", // must be skipped
		"",              // trailing NUL
	}
	data := []byte(strings.Join(fields, "\x00"))

	branch, upstream, ahead, behind, files := parseStatusPorcelainV2(data)
	if branch != "main" {
		t.Errorf("branch = %q, want main", branch)
	}
	if upstream != "origin/main" {
		t.Errorf("upstream = %q, want origin/main", upstream)
	}
	if ahead != 2 || behind != 1 {
		t.Errorf("ahead/behind = %d/%d, want 2/1", ahead, behind)
	}
	want := []FileStatus{
		{Path: "staged.go", Staged: "M", Worktree: "."},
		{Path: "dirty.go", Staged: ".", Worktree: "M"},
		{Path: "new.go", Staged: "R", Worktree: ".", RenamedFrom: "old.go"},
		{Path: "untracked.txt", Staged: "?", Worktree: "?"},
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("files =\n%#v\nwant\n%#v", files, want)
	}
}

func TestParseStatusEmpty(t *testing.T) {
	// A clean repo on a detached head: only branch headers, no changes.
	data := []byte(strings.Join([]string{
		"# branch.head (detached)",
		"# branch.ab +0 -0",
		"",
	}, "\x00"))
	branch, upstream, ahead, behind, files := parseStatusPorcelainV2(data)
	if branch != "(detached)" {
		t.Errorf("branch = %q", branch)
	}
	if upstream != "" || ahead != 0 || behind != 0 {
		t.Errorf("unexpected upstream/ahead/behind: %q %d %d", upstream, ahead, behind)
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
}

func TestParseLog(t *testing.T) {
	rec := func(parts ...string) string { return strings.Join(parts, "\x00") + "\x1e" }
	data := []byte(
		rec("h1", "p0", "Alice", "2026-07-23T10:00:00+00:00", "HEAD -> main, origin/main", "first") +
			"\n" + rec("h2", "p1 p2", "Bob", "2026-07-22T09:00:00+00:00", "tag: v1.0", "a merge") +
			"\n",
	)
	commits, truncated := parseLog(data)
	if truncated {
		t.Error("two commits must not be truncated")
	}
	if len(commits) != 2 {
		t.Fatalf("len(commits) = %d, want 2", len(commits))
	}
	if commits[0].Hash != "h1" || commits[0].Author != "Alice" || commits[0].Subject != "first" {
		t.Errorf("commit0 = %#v", commits[0])
	}
	if !reflect.DeepEqual(commits[0].Parents, []string{"p0"}) {
		t.Errorf("commit0 parents = %v", commits[0].Parents)
	}
	if !reflect.DeepEqual(commits[0].Refs, []string{"main", "origin/main"}) {
		t.Errorf("commit0 refs = %v, want [main origin/main]", commits[0].Refs)
	}
	if !reflect.DeepEqual(commits[1].Parents, []string{"p1", "p2"}) {
		t.Errorf("commit1 parents = %v, want [p1 p2]", commits[1].Parents)
	}
	if !reflect.DeepEqual(commits[1].Refs, []string{"v1.0"}) {
		t.Errorf("commit1 refs = %v, want [v1.0]", commits[1].Refs)
	}
}

func TestParseRefs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"HEAD", nil},
		{"HEAD -> main", []string{"main"}},
		{"HEAD -> main, origin/main, tag: v1.0", []string{"main", "origin/main", "v1.0"}},
		{"tag: v2.3.4", []string{"v2.3.4"}},
	}
	for _, tt := range tests {
		if got := parseRefs(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseRefs(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseAheadBehind(t *testing.T) {
	tests := []struct {
		in           string
		wantA, wantB int
	}{
		{"+0 -0", 0, 0},
		{"+3 -5", 3, 5},
		{"", 0, 0},
	}
	for _, tt := range tests {
		a, b := parseAheadBehind(tt.in)
		if a != tt.wantA || b != tt.wantB {
			t.Errorf("parseAheadBehind(%q) = %d/%d, want %d/%d", tt.in, a, b, tt.wantA, tt.wantB)
		}
	}
}

// TestEmptyInfoWireShape asserts the array-contract invariant: a normalized
// snapshot marshals status/log as [] (never null), and each commit's
// parents/refs as [] (never null). Finding 7.
func TestEmptyInfoWireShape(t *testing.T) {
	b, err := json.Marshal(EmptyInfo())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"status":[]`, `"log":[]`, `"status_truncated":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("EmptyInfo JSON = %s, want substring %q", got, want)
		}
	}

	// A root commit (no parents) + an undecorated commit (no refs), normalized,
	// must encode parents:[] and refs:[] rather than null.
	info := normalizeInfo(Info{
		IsGit: true,
		Log: []Commit{
			{Hash: "root", Parents: nil, Refs: nil, Subject: "init"},
		},
	})
	b, err = json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	got = string(b)
	for _, want := range []string{`"parents":[]`, `"refs":[]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized commit JSON = %s, want substring %q", got, want)
		}
	}
}

// TestSnapshotWireShapeClean builds a real one-commit repo and asserts the wire
// shape a clean repository produces: status:[] (no changes), the root commit's
// parents:[], and an undecorated commit's refs:[]. Finding 7 end-to-end.
func TestSnapshotWireShapeClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e.co",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e.co")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("checkout", "-q", "-b", "main")
	// Two commits: the newest carries the "main" ref decoration; the ROOT commit
	// (older, in a clean tree) is undecorated AND parentless, so it must encode
	// refs:[] and parents:[] rather than null.
	run("commit", "-q", "--allow-empty", "-m", "root")
	run("commit", "-q", "--allow-empty", "-m", "second")

	info, err := Snapshot(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// Clean repo → status:[]; root commit → parents:[]; undecorated root → refs:[].
	for _, want := range []string{`"status":[]`, `"parents":[]`, `"refs":[]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("Snapshot JSON = %s, want substring %q", got, want)
		}
	}
	if strings.Contains(got, `:null`) {
		t.Fatalf("Snapshot JSON contains a null (array-contract violation): %s", got)
	}
}

// TestSnapshotIntegration builds a throwaway repo and exercises the real git
// binary end-to-end. It is skipped when git is unavailable.
func TestSnapshotIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(
			cmd.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Non-git dir first.
	if info, err := Snapshot(ctx, dir); err != nil || info.IsGit {
		t.Fatalf("pre-init Snapshot: info.IsGit=%v err=%v, want false/nil", info.IsGit, err)
	}

	run("init", "-q")
	run("checkout", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "initial commit")

	// A tracked change + an untracked file.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-q", "-m", "add tracked")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Snapshot(ctx, dir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !info.IsGit {
		t.Fatal("IsGit = false, want true")
	}
	if info.Branch != "main" {
		t.Errorf("branch = %q, want main", info.Branch)
	}
	if len(info.Log) != 2 {
		t.Errorf("log has %d commits, want 2", len(info.Log))
	}
	if info.Log[0].Subject != "add tracked" {
		t.Errorf("newest subject = %q, want 'add tracked'", info.Log[0].Subject)
	}
	var haveTrackedMod, haveUntracked bool
	for _, f := range info.Status {
		if f.Path == "tracked.txt" {
			haveTrackedMod = true
		}
		if f.Path == "untracked.txt" && f.Staged == "?" {
			haveUntracked = true
		}
	}
	if !haveTrackedMod {
		t.Errorf("status missing modified tracked.txt: %+v", info.Status)
	}
	if !haveUntracked {
		t.Errorf("status missing untracked.txt: %+v", info.Status)
	}
}
