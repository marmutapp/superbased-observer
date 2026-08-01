// Package conformast extracts the DOMAIN of an adapter's private
// action-type classifier straight out of its source, by AST walk, so a
// conformance test can prove `private classifier ⊆ tooltax` instead of
// merely proving the two agree where they happen to overlap.
//
// Why this exists (WP-T3 codex review, 2026-07-31): the first cut of the
// per-adapter conformance tests iterated the tooltax table and skipped any
// native name the adapter's classifier did not recognise. That direction
// alone is bypassable — adding
//
//	case "new_native": return models.ActionRunCommand
//
// to a classifier with no matching tooltax row left every test green,
// because the loop never visits a name tooltax has never heard of. The
// missing half is the classifier's own domain, and the only honest source
// for it is the code itself: these switches are not enumerable at runtime.
//
// The package is TEST INFRASTRUCTURE and pure: go/ast + go/parser only, no
// SQL, no HTTP, no fsnotify, and no dependency on internal/tooltax itself
// (the caller joins the two). It reads .go files under a directory, which
// is inherent to what it does — an AST walk of the tree under test, the
// same tactic tests/invariant/privacy_test.go's sentinel uses.
//
// Usage from an adapter's own package test (cwd is the package dir):
//
//	domain, err := conformast.SwitchCaseStrings(".", "mapToolName")
//
// then feed every name in domain through the REAL classifier and assert
// tooltax carries a tool-specific row that agrees. AST supplies the
// domain; the classifier supplies the values; together they are the subset
// proof.
package conformast
