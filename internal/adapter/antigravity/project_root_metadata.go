package antigravity

import (
	"regexp"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/platform/pathnorm"
)

// extractProjectRootFromTranscript walks transcript entries for a
// USER_INPUT containing an <ADDITIONAL_METADATA> block, parses the
// "Active Document" path (and "Other open documents" lines as
// fallback candidates), translates them via pathnorm.Normalize to
// WSL-canonical form, walks each one up via git.FindRoot, and
// returns the first git root found.
//
// Returns "" when no transcript entry contains parseable metadata or
// when none of the parsed paths sit inside a git working tree.
//
// Why: when state.vscdb's trajectorySummaries blob doesn't yet have
// the conversation (Antigravity flushes the index periodically, not
// on every turn), lookupIndexEntry returns nil and projectRoot would
// otherwise fall back to the "[antigravity]" placeholder for the
// dashboard's Project column. The IDE's own metadata stamp in every
// USER_INPUT carries the active workspace context — using it
// recovers the real project attribution without waiting for the next
// state.vscdb flush.
//
// Verified shape from the operator's f7c9f417 (2026-05-24):
//
//	<USER_REQUEST>...</USER_REQUEST>
//	<ADDITIONAL_METADATA>
//	The current local time is: 2026-05-24T12:15:55+05:30.
//
//	The user's current state is as follows:
//	Active Document: c:\programsx\regulation\regulation-saas\scripts\security\README.md (LANGUAGE_MARKDOWN)
//	Cursor is on line: 1
//	Other open documents:
//	- c:\programsx\regulation\regulation-saas\scripts\02_session_persistence.sql (LANGUAGE_UNSPECIFIED)
//	- c:\programsx\regulation\regulation-saas\src\app\(dashboard)\reports\page.tsx (LANGUAGE_TSX)
//	...
//	</ADDITIONAL_METADATA>
func extractProjectRootFromTranscript(entries []cliTranscriptEntry) string {
	for _, e := range entries {
		if e.Source != "USER_EXPLICIT" || e.Type != "USER_INPUT" {
			continue
		}
		if root := extractProjectRootFromMetadata(e.Content); root != "" {
			return root
		}
	}
	return ""
}

// extractProjectRootFromMetadata is the per-content worker.
// Exposed separately so tests can exercise the parsing logic without
// constructing transcript entry fixtures.
func extractProjectRootFromMetadata(content string) string {
	for _, p := range candidateDocumentPaths(content) {
		normalized := pathnorm.Normalize(p)
		if normalized == "" {
			continue
		}
		if root, ok := git.FindRoot(normalized); ok {
			return root
		}
	}
	return ""
}

// activeDocumentLine matches the "Active Document: <path> (<LANG>)"
// line emitted by the IDE. Captures only the path portion — strips
// the trailing " (LANGUAGE_…)" annotation.
var activeDocumentLine = regexp.MustCompile(`(?m)^Active Document:\s+(.+?)(?:\s+\([A-Z_]+\))?\s*$`)

// otherDocumentLine matches each entry in the "Other open
// documents:" bullet list. Format: leading "- ", then the absolute
// path, then optional " (LANGUAGE_…)".
var otherDocumentLine = regexp.MustCompile(`(?m)^-\s+(.+?)(?:\s+\([A-Z_]+\))?\s*$`)

// candidateDocumentPaths returns Active Document first, then Other
// open documents in transcript order. Ordering matters: the Active
// Document is the most likely to share a git root with the rest of
// the workspace, so trying it first lets the cheap success path
// avoid the second / third / Nth git walk in the typical case.
func candidateDocumentPaths(content string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if m := activeDocumentLine.FindStringSubmatch(content); len(m) == 2 {
		add(m[1])
	}
	for _, m := range otherDocumentLine.FindAllStringSubmatch(content, -1) {
		if len(m) == 2 {
			add(m[1])
		}
	}
	return out
}
