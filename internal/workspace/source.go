package workspace

import "fmt"

// Source is the closed vocabulary of workspace-preparation strategies a
// sandboxed terminal run can request (plan §4).
type Source string

const (
	// SourceLive is the degenerate case: the workspace IS the caller's
	// already-validated project root, no copy is made.
	SourceLive Source = "live"
	// SourceCloneLocal produces a self-contained local clone
	// (`--no-hardlinks`) of ProjectRoot.
	SourceCloneLocal Source = "clone-local"
	// SourceCloneRemote clones RemoteURL host-side with the operator's
	// ambient git auth. Gated by AllowRemoteClone.
	SourceCloneRemote Source = "clone-remote"
	// SourceWorktree adds a `git worktree` off ProjectRoot. Gated by
	// AllowWorktreeSource — it requires ProjectRoot's own .git bound
	// read-write and attributes the run to the main repo.
	SourceWorktree Source = "worktree"
)

// ParseSource validates s against the closed Source vocabulary. It never
// fabricates a source for an unrecognized value.
func ParseSource(s string) (Source, error) {
	switch Source(s) {
	case SourceLive, SourceCloneLocal, SourceCloneRemote, SourceWorktree:
		return Source(s), nil
	default:
		return "", fmt.Errorf("workspace.ParseSource: unknown source %q (want one of %q, %q, %q, %q)",
			s, SourceLive, SourceCloneLocal, SourceCloneRemote, SourceWorktree)
	}
}
