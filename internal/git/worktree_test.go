package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitAvailable skips the battery when the git binary is missing so the suite
// stays green on stripped-down hosts.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// initRepo creates a real git repo with one commit so worktree operations
// have a HEAD to branch from.
func initRepo(t *testing.T) string {
	t.Helper()
	gitAvailable(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	return dir
}

func TestIsDirty(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	dirty, err := IsDirty(ctx, repo)
	if err != nil || dirty {
		t.Fatalf("fresh repo dirty=%v err=%v, want false nil", dirty, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = IsDirty(ctx, repo)
	if err != nil || !dirty {
		t.Fatalf("dirty=%v err=%v after untracked write, want true nil", dirty, err)
	}
}

func TestHeadSHABranch(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	sha, err := HeadSHA(ctx, repo)
	if err != nil || len(sha) != 40 {
		t.Fatalf("HeadSHA=%q err=%v, want 40-char sha", sha, err)
	}
	branch, err := CurrentBranch(ctx, repo)
	if err != nil || branch == "" {
		t.Fatalf("CurrentBranch=%q err=%v, want non-empty", branch, err)
	}
}

func TestWorktreeRoundtrip(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")

	if err := WorktreeAdd(ctx, repo, wtPath, "arena/t1", "HEAD"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Fatalf("worktree checkout incomplete: %v", err)
	}
	branch, err := CurrentBranch(ctx, wtPath)
	if err != nil || branch != "arena/t1" {
		t.Fatalf("worktree branch=%q err=%v, want arena/t1", branch, err)
	}
	if err := WorktreeRemove(ctx, repo, wtPath); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present after remove: %v", err)
	}
	if err := DeleteBranch(ctx, repo, "arena/t1"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if err := DeleteBranch(ctx, repo, "arena/t1"); err != nil {
		t.Fatalf("DeleteBranch must converge when already absent: %v", err)
	}
}

func TestCommitAllCapturesChanges(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAdd(ctx, repo, wtPath, "arena/t2", "HEAD"); err != nil {
		t.Fatal(err)
	}

	baseSHA, _ := HeadSHA(ctx, repo)
	files, added, removed, err := DiffStat(ctx, wtPath, baseSHA)
	if err != nil || files != 0 {
		t.Fatalf("clean diff stat files=%d added=%d removed=%d err=%v, want zeros", files, added, removed, err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "code.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(wtPath, "gone.txt")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sha1, err := CommitAll(ctx, wtPath, "candidate edits")
	if err != nil || len(sha1) != 40 {
		t.Fatalf("CommitAll sha=%q err=%v", sha1, err)
	}
	if sha1 == baseSHA {
		t.Fatal("commit SHA unchanged after edits")
	}

	// Deletion round: remove code.go, capture again.
	if err := os.Remove(filepath.Join(wtPath, "code.go")); err != nil {
		t.Fatal(err)
	}
	sha2, err := CommitAll(ctx, wtPath, "delete code.go")
	if err != nil || len(sha2) != 40 || sha2 == sha1 {
		t.Fatalf("second CommitAll sha=%q err=%v (prev %s)", sha2, err, sha1)
	}
	files, _, _, err = DiffStat(ctx, wtPath, baseSHA)
	if err != nil {
		t.Fatalf("DiffStat vs base: %v", err)
	}
	// Net diff vs base is README.md only: code.go was added by the first
	// commit and deleted by the second, so it cancels out.
	if files != 1 {
		t.Fatalf("diff files=%d vs base, want 1", files)
	}
}

func TestCommitAllAllowEmpty(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAdd(ctx, repo, wtPath, "arena/t3", "HEAD"); err != nil {
		t.Fatal(err)
	}
	baseSHA, _ := HeadSHA(ctx, repo)
	sha, err := CommitAll(ctx, wtPath, "no-op candidate")
	if err != nil {
		t.Fatalf("allow-empty CommitAll: %v", err)
	}
	if sha == baseSHA {
		t.Fatal("allow-empty commit did not advance HEAD")
	}
}

func TestPatchTextContainsChanges(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAdd(ctx, repo, wtPath, "arena/t4", "HEAD"); err != nil {
		t.Fatal(err)
	}
	baseSHA, _ := HeadSHA(ctx, repo)
	body := "# rewritten\nmore lines\n"
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, err := PatchText(ctx, wtPath, baseSHA)
	if err != nil {
		t.Fatalf("PatchText: %v", err)
	}
	if !strings.Contains(patch, "+# rewritten") {
		t.Fatalf("patch missing added content:\n%s", patch)
	}
}

func TestDetachedHeadBranchEmpty(t *testing.T) {
	repo := initRepo(t)
	gitAvailable(t)
	ctx := context.Background()
	sha, _ := HeadSHA(ctx, repo)
	cmd := exec.Command("git", "checkout", "--detach", sha)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detach: %v: %s", err, out)
	}
	branch, err := CurrentBranch(ctx, repo)
	if err != nil || branch != "" {
		t.Fatalf("detached CurrentBranch=%q err=%v, want \"\" nil", branch, err)
	}
}
