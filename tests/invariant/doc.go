// Package invariant is the regression net for the load-bearing
// single-user-local invariant of the Teams & Org Visibility feature:
// a user who never enrols must see byte-identical dashboard behaviour.
//
// The suite seeds a deterministic corpus into a fresh observer database,
// drives the single-user dashboard's HTTP handler for the headline
// endpoints (Overview, Sessions, Actions, Tools, Cost), canonicalises
// each JSON response (absolute timestamps tokenised, map keys sorted),
// and diffs it against a golden captured before the org-mode code landed.
// With the org-mode changes applied but no agent enrolled, the diff must
// be empty — the new columns serialise to NULL and are not projected by
// any existing endpoint.
//
// Regenerate the goldens intentionally with:
//
//	go test ./tests/invariant -update
//
// or via `make test-invariant` for the verifying (non-update) run.
package invariant
