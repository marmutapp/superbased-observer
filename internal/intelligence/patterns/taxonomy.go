package patterns

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// The canonical action-type sets the derivation queries scope on.
//
// WP-T5 replaced six inline SQL literal lists (`action_type IN
// ('read_file', 'edit_file', 'write_file')` and friends) with these, so
// the Patterns engine, the dashboard and the adapters agree on what "a
// file action" is BY CONSTRUCTION rather than by six copies of the same
// three strings staying in sync by hand. This is pure de-duplication: the
// derived sets are pinned byte-for-byte against the pre-WP-T5 literals in
// taxonomy_test.go, and any tooltax change that moves a set fails that
// pin loudly instead of silently changing what a pattern means.
//
// Per-site decisions (each site's SQL carries a matching comment):
//
//   - crossTool / hotFiles scope on the WHOLE file category, which is
//     exactly {edit_file, read_file, write_file} — plain equality.
//   - coChange / editTestPairs scope on the file category MINUS
//     read_file: they are about files an agent CHANGED, and a read is
//     not a change. Expressed as a subtraction from the category (not a
//     hand-listed pair) so a future mutating file action type is a
//     deliberate decision at the pin, not an omission nobody notices.
//   - commonCommands / editTestPairs scope on the whole cmd category,
//     which is exactly {run_command} — plain equality.
//   - onboardingSequences scopes on read_file ALONE. That is one action
//     type, not a category (the file category would wrongly pull in
//     edits), so it uses the tooltax constant directly.
var (
	// fileActionTypes is the canonical file category: every action type
	// that touches a file, read or write.
	fileActionTypes = tooltax.ActionTypesInCategory(tooltax.CategoryFile)

	// mutatingFileActionTypes is the file category minus read_file — the
	// actions that CHANGE a file.
	mutatingFileActionTypes = withoutActionType(fileActionTypes, tooltax.ActionReadFile)

	// commandActionTypes is the canonical cmd category.
	commandActionTypes = tooltax.ActionTypesInCategory(tooltax.CategoryCmd)

	// mutatingFileOrCommandActionTypes is the union the edit→test pair
	// scan needs: the file mutations it brackets and the commands it
	// brackets them with.
	mutatingFileOrCommandActionTypes = unionActionTypes(mutatingFileActionTypes, commandActionTypes)
)

// withoutActionType returns src minus one action type, preserving order.
func withoutActionType(src []string, drop string) []string {
	out := make([]string, 0, len(src))
	for _, s := range src {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// unionActionTypes concatenates action-type sets, dropping duplicates and
// preserving first-seen order (the inputs are already sorted within
// themselves, which is all the determinism the SQL text needs).
func unionActionTypes(sets ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 8)
	for _, set := range sets {
		for _, s := range set {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// inPlaceholders renders the `?, ?, ?` body of a SQL IN list for n bound
// values. n == 0 yields "NULL", so an empty set matches NOTHING rather
// than producing `IN ()` (a syntax error) or, worse, tempting a caller to
// drop the predicate and match everything.
func inPlaceholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// bindArgs widens an action-type set into query arguments.
func bindArgs(vals []string) []any {
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		out = append(out, v)
	}
	return out
}
