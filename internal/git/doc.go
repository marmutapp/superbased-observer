// Package git resolves a project's git root, current branch, and remote URL
// and produces project-relative paths for targets. All observer data is
// organized by git root (spec §20). Implementation uses os.Stat walks up for
// .git — no external git binary calls on the hot path.
package git
