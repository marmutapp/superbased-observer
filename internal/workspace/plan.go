package workspace

import (
	"fmt"
	"path/filepath"
)

// Step is one git invocation the caller's injected runner must execute.
// Argv[0] is always "git". Dir is the working directory to invoke the
// process from; empty means the command is fully path-qualified (via a
// `-C <dir>` git flag or absolute positional arguments) so the runner's
// own default working directory is fine.
type Step struct {
	Argv []string
	Dir  string
}

// Steps is Plan's result: the resolved final workspace directory (Dir)
// plus the ordered git Steps that produce it (empty for SourceLive). The
// caller executes Steps in order via its own injected subprocess runner
// — this package never calls os/exec — and stops at the first failure.
type Steps struct {
	Dir   string
	Steps []Step
}

// Plan is the pure decision at the heart of U3 workspace preparation
// (docs/plans/b9-sandboxed-terminals-implementation-plan-2026-08-08.md
// §4). It computes the destination path and the ordered git argv needed
// to produce it, validating every path/token that will reach a
// subprocess argv along the way. It never execs anything; the caller
// supplies its own git runner and invokes it once per returned Step, in
// order, stopping at the first failure.
func Plan(req Request) (Steps, error) {
	switch req.Source {
	case SourceLive:
		return planLive(req)
	case SourceCloneLocal:
		return planCloneLocal(req)
	case SourceCloneRemote:
		return planCloneRemote(req)
	case SourceWorktree:
		return planWorktree(req)
	default:
		return Steps{}, fmt.Errorf("workspace.Plan: unknown source %q", req.Source)
	}
}

// planLive is the ruled degenerate case: no copy, no git steps. The
// workspace IS the validated project root.
func planLive(req Request) (Steps, error) {
	if err := validateAbsPath("ProjectRoot", req.ProjectRoot); err != nil {
		return Steps{}, err
	}
	return Steps{Dir: req.ProjectRoot}, nil
}

// planCloneLocal composes
//
//	git clone --no-hardlinks -- <ProjectRoot> <dest>
//
// --no-hardlinks is load-bearing (mutation proof #5): git's default
// --local clone hardlinks object files, so the sandboxed agent's own
// `git gc`/repack inside the workspace could mutate objects shared with
// the REAL repo's object store. Dropping the flag must fail
// TestCloneLocalUsesNoHardlinks.
func planCloneLocal(req Request) (Steps, error) {
	if err := validateAbsPath("ProjectRoot", req.ProjectRoot); err != nil {
		return Steps{}, err
	}
	dest, err := mintDest(req.ManagedRoot, req.ID, filepath.Base(req.ProjectRoot))
	if err != nil {
		return Steps{}, err
	}
	steps := []Step{
		{Argv: []string{"git", "clone", "--no-hardlinks", "--", req.ProjectRoot, dest}},
	}
	if req.Branch != "" {
		if err := validateCleanToken("Branch", req.Branch); err != nil {
			return Steps{}, err
		}
		steps = append(steps, Step{Argv: []string{"git", "-C", dest, "checkout", "-b", req.Branch}})
	}
	return Steps{Dir: dest, Steps: steps}, nil
}

// planCloneRemote composes `git clone -- <url> <dest>`, run host-side
// with the operator's ambient auth. Gated by AllowRemoteClone; the URL
// is validated by ValidateRemoteURL before it ever reaches an argv.
func planCloneRemote(req Request) (Steps, error) {
	if !req.AllowRemoteClone {
		return Steps{}, fmt.Errorf("workspace.Plan: clone-remote source requires allow_remote_clone (the daemon runs `git clone <url>` with your ambient auth, so it is opt-in)")
	}
	if err := ValidateRemoteURL(req.RemoteURL, req.RemoteAllowedHosts); err != nil {
		return Steps{}, err
	}
	dest, err := mintDest(req.ManagedRoot, req.ID, repoLeafFromURL(req.RemoteURL))
	if err != nil {
		return Steps{}, err
	}
	steps := []Step{
		{Argv: []string{"git", "clone", "--", req.RemoteURL, dest}},
	}
	if req.Branch != "" {
		if err := validateCleanToken("Branch", req.Branch); err != nil {
			return Steps{}, err
		}
		steps = append(steps, Step{Argv: []string{"git", "-C", dest, "checkout", "-b", req.Branch}})
	}
	return Steps{Dir: dest, Steps: steps}, nil
}

// planWorktree composes
//
//	git -C <ProjectRoot> worktree add -b <Branch> -- <dest>
//
// Gated by AllowWorktreeSource — a worktree needs ProjectRoot's own
// .git bound read-write inside the sandbox and attributes the run to the
// main repo (git.FindRoot resolves the worktree's .git FILE back to the
// main repo), a caveat the caller surfaces in the dashboard copy. Branch
// is required: without a distinct branch, `git worktree add` collides
// with whatever branch ProjectRoot already has checked out.
func planWorktree(req Request) (Steps, error) {
	if !req.AllowWorktreeSource {
		return Steps{}, fmt.Errorf("workspace.Plan: worktree source requires allow_worktree_source (it needs the main repo's .git bound read-write, and attribution points at the main repo, not the sandboxed copy — off by default)")
	}
	if err := validateAbsPath("ProjectRoot", req.ProjectRoot); err != nil {
		return Steps{}, err
	}
	if req.Branch == "" {
		return Steps{}, fmt.Errorf("workspace.Plan: worktree source requires Branch (a worktree without a distinct branch collides with the source checkout)")
	}
	if err := validateCleanToken("Branch", req.Branch); err != nil {
		return Steps{}, err
	}
	dest, err := mintDest(req.ManagedRoot, req.ID, filepath.Base(req.ProjectRoot))
	if err != nil {
		return Steps{}, err
	}
	steps := []Step{
		{Argv: []string{"git", "-C", req.ProjectRoot, "worktree", "add", "-b", req.Branch, "--", dest}},
	}
	return Steps{Dir: dest, Steps: steps}, nil
}

// mintDest computes <managedRoot>/<id>/<leafCandidate> and validates
// every component: managedRoot must be an absolute path, id must be a
// clean single path segment, and leafCandidate (the caller-derived
// candidate repo directory name) is validated the same way — an
// empty/"."/".." or otherwise unsafe candidate is REJECTED, never
// silently substituted. The result additionally passes validateAbsPath
// as a final sanity check; ValidateManagedWorkspace re-verifies
// containment once the caller has resolved the real filesystem path.
func mintDest(managedRoot, id, leafCandidate string) (string, error) {
	if err := validateAbsPath("ManagedRoot", managedRoot); err != nil {
		return "", err
	}
	if err := validateCleanToken("ID", id); err != nil {
		return "", err
	}
	if err := validateCleanToken("repoLeaf", leafCandidate); err != nil {
		return "", fmt.Errorf("workspace: could not derive a workspace directory name: %w", err)
	}
	dest := filepath.Join(managedRoot, id, leafCandidate)
	if err := validateAbsPath("dest", dest); err != nil {
		return "", err
	}
	return dest, nil
}
