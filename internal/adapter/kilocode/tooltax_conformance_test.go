package kilocode

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
	"github.com/marmutapp/superbased-observer/internal/tooltax/conformast"
)

// classifyForTooltax runs the adapter's REAL private classifier over one
// native tool name. It is the single place the two conformance tests below
// reach into the shipping code, so neither can drift into testing a
// re-implementation.
//
// The classifier is mapTool (adapter.go), the CLI adapter's `switch strings.ToLower(strings.TrimSpace(part.Tool))` (the LEGACY kilo-code adapter wraps internal/adapter/cline and is pinned there).
func classifyForTooltax(native string) string {
	return func() string { a, _, _, _ := mapTool(toolPartData{Tool: native}); return a }()
}

// tooltaxTools lists the tool ids this adapter's classifier feeds.
var tooltaxTools = []string{models.ToolKiloCodeCLI}

// TestPrivateMappingAgreesWithTooltax is the TABLE→CLASSIFIER half of the
// WP-T3 conformance pin (plan of record:
// docs/plans/tool-taxonomy-standardization-plan-2026-07-31.md §2). It walks
// every tool-specific tooltax row and asserts the adapter, where it
// classifies that name at all, agrees.
//
// On its own this direction is NOT a subset proof — it never visits a name
// tooltax has not heard of. TestClassifierDomainIsSubsetOfTooltax below is
// the other half and closes that hole.
func TestPrivateMappingAgreesWithTooltax(t *testing.T) {
	for _, tool := range tooltaxTools {
		checked := 0
		for _, e := range tooltax.Table() {
			if e.Tool != tool || e.IsGlob() {
				continue
			}
			got := classifyForTooltax(e.Native)
			if got == "" || got == models.ActionUnknown {
				// The adapter does not classify this name. tooltax
				// knowing more is the ALLOWED direction.
				continue
			}
			checked++
			if got != e.ActionType {
				t.Errorf("drift: %s %q — adapter classifies as %q, tooltax table says %q",
					tool, e.Native, got, e.ActionType)
			}
		}
		if checked == 0 {
			t.Errorf("no %s row in the tooltax table reached the adapter's classifier — "+
				"the pin is vacuous (did the tool id or the classifier change?)", tool)
		}
	}
}

// unclassifiedDomain freezes the case labels that resolve to
// ActionUnknown when fed back through the classifier. Each is one of two
// things, and the value says which:
//
//   - a DELIBERATE unknown — a tool we have seen and consciously chose not
//     to bucket, which is different information from "never seen";
//   - a DEAD case — the classifier normalizes its input before switching,
//     and this label is a spelling that normalization can never produce.
//
// Both are findings a reviewer must acknowledge, which is why the set is
// frozen: a NEW entry appearing here fails the test until someone writes
// down which of the two it is.
var unclassifiedDomain = map[string]string{}

// missingTooltaxRows freezes case labels the classifier DOES map but for
// which internal/tooltax carries no tool-specific row yet. Each entry is a
// real, acknowledged gap in the canonical table — never a licence to add
// more: a new unlisted name fails the test, which is the whole point of the
// domain direction.
//
// The staleness pin below fails once the row IS added, forcing the freeze
// to be deleted rather than quietly outliving the gap.
var missingTooltaxRows = map[string]string{}

// TestClassifierDomainIsSubsetOfTooltax is the CLASSIFIER→TABLE half, and
// the one that makes this a real subset proof. It extracts the classifier's
// DOMAIN from source by AST walk (internal/tooltax/conformast) — these
// switches are not enumerable at runtime — then feeds every extracted label
// back through the REAL classifier and requires a tool-specific tooltax row
// that agrees.
//
// This is the direction that catches the concrete bypass the codex review
// found: adding `case "new_native": return models.ActionRunCommand` with no
// matching tooltax row. The table-walking test above cannot see it; this one
// fails on it.
func TestClassifierDomainIsSubsetOfTooltax(t *testing.T) {
	domain, err := conformast.SwitchCaseStrings(".", "mapTool")
	if err != nil {
		t.Fatalf("extracting the classifier domain: %v", err)
	}
	if len(domain) < 3 {
		t.Fatalf("extracted only %d case labels (%q) — too few to be the real domain; "+
			"the classifier shape probably changed", len(domain), domain)
	}
	inDomain := make(map[string]bool, len(domain))
	for _, d := range domain {
		inDomain[d] = true
	}
	for _, native := range domain {
		got := classifyForTooltax(native)
		if got == "" || got == models.ActionUnknown {
			if _, frozen := unclassifiedDomain[native]; !frozen {
				t.Errorf("case %q resolves to %q when fed back through the classifier — "+
					"either a DELIBERATE unknown or a DEAD case whose label the "+
					"classifier's own normalization can never produce; say which in "+
					"unclassifiedDomain", native, got)
			}
			continue
		}
		for _, tool := range tooltaxTools {
			e, ok := tooltax.Resolve(tool, native)
			if !ok || e.Tool != tool {
				if _, known := missingTooltaxRows[native]; known {
					continue
				}
				t.Errorf("the classifier maps %q → %q but internal/tooltax has no "+
					"%s-specific row for it — add the name to tooltax's table",
					native, got, tool)
				continue
			}
			if got != e.ActionType {
				t.Errorf("drift: %s %q — classifier says %q, tooltax says %q",
					tool, native, got, e.ActionType)
			}
		}
	}
	for native := range unclassifiedDomain {
		if !inDomain[native] {
			t.Errorf("unclassifiedDomain entry %q is no longer a case label of the "+
				"classifier — remove the stale freeze", native)
		}
	}
	for native := range missingTooltaxRows {
		if !inDomain[native] {
			t.Errorf("missingTooltaxRows entry %q is no longer a case label of the "+
				"classifier — remove the stale freeze", native)
			continue
		}
		for _, tool := range tooltaxTools {
			if e, ok := tooltax.Resolve(tool, native); ok && e.Tool == tool {
				t.Errorf("missingTooltaxRows still lists %q but tooltax now carries a "+
					"%s-specific row for it — delete the freeze", native, tool)
			}
		}
	}
}
