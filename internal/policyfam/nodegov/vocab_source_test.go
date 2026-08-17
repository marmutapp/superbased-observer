package nodegov

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The adversarial review's D-C: schema 1 makes an unknown section id a HARD
// publish-time lint error and an agent-side decode failure — deliberately,
// because silently ignoring one would let an admin believe a page is hidden
// when it is not. That choice turns any hand-copied vocabulary into a
// fleet-wide outage waiting for a page rename. These two tests read the SPA
// sources and pin the vocabularies to them, so a rename fails HERE (with the
// exact list to update) instead of at an org's publish endpoint.
//
// They are asserts, not code generation: generating Go from TSX at build
// time would put a parser on the build path for two short literal lists.

const (
	navSourceRel      = "../../../web/src/lib/nav.ts"
	settingsSourceRel = "../../../web/src/pages/Settings.tsx"
)

// navItemRe matches a single nav ITEM line ({ id: "x", label: ..., path: ...}).
// The `path:` requirement is what excludes the NAV_GROUPS group ids, which
// are not pages and are therefore not part of the vocabulary.
// The [^{}]* class is load-bearing: it stops the match from running out of a
// NAV_GROUPS group object (whose own `id:` is followed, eventually, by its
// first item's `path:`) and mistaking a group id for a page id.
var navItemRe = regexp.MustCompile(`\{\s*id:\s*"([a-z0-9-]+)"[^{}]*path:\s*"`)

// settingsIDRe matches the `id: "x",` line of a SectionDef inside the
// SECTIONS array literal.
var settingsIDRe = regexp.MustCompile(`^\s*id:\s*"([a-z0-9-]+)",\s*$`)

func TestNavSectionIDsMatchNavTS(t *testing.T) {
	raw := readSPASource(t, navSourceRel)
	var got []string
	for _, m := range navItemRe.FindAllStringSubmatch(raw, -1) {
		got = append(got, m[1])
	}
	if len(got) == 0 {
		t.Fatalf("parsed no nav item ids out of %s — the SPA file's shape changed and this test's regex needs updating BEFORE the vocabulary can be trusted", navSourceRel)
	}
	assertSameSet(t, "NavSectionIDs", NavSectionIDs, got, navSourceRel)
}

func TestSettingsSectionIDsMatchSettingsTSX(t *testing.T) {
	raw := readSPASource(t, settingsSourceRel)
	block := sectionsArrayBlock(t, raw)
	var got []string
	for _, line := range strings.Split(block, "\n") {
		if m := settingsIDRe.FindStringSubmatch(line); m != nil {
			got = append(got, m[1])
		}
	}
	if len(got) == 0 {
		t.Fatalf("parsed no settings section ids out of %s — the SPA file's shape changed and this test's regex needs updating BEFORE the vocabulary can be trusted", settingsSourceRel)
	}
	assertSameSet(t, "SettingsSectionIDs", SettingsSectionIDs, got, settingsSourceRel)
}

// TestUnhideableSetsAreDrawnFromTheVocabularies keeps the T8 floor honest: an
// unhideable id that is not a real section would silently protect nothing.
func TestUnhideableSetsAreDrawnFromTheVocabularies(t *testing.T) {
	for _, id := range UnhideableNavSectionIDs {
		if !IsNavSection(id) {
			t.Errorf("UnhideableNavSectionIDs names %q, which is not in NavSectionIDs — it would protect nothing", id)
		}
	}
	for _, id := range UnhideableSettingsSectionIDs {
		if !IsSettingsSection(id) {
			t.Errorf("UnhideableSettingsSectionIDs names %q, which is not in SettingsSectionIDs — it would protect nothing", id)
		}
	}
}

func readSPASource(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// sectionsArrayBlock extracts the SECTIONS array literal so group ids and
// other `id:` literals elsewhere in Settings.tsx are not mistaken for
// sections.
func sectionsArrayBlock(t *testing.T, raw string) string {
	const marker = "const SECTIONS: SectionDef[] = ["
	i := strings.Index(raw, marker)
	if i < 0 {
		t.Fatalf("%s no longer contains %q — update this test with the new declaration BEFORE trusting the vocabulary", settingsSourceRel, marker)
	}
	rest := raw[i+len(marker):]
	end := strings.Index(rest, "\n];")
	if end < 0 {
		t.Fatalf("could not find the end of the SECTIONS array in %s", settingsSourceRel)
	}
	return rest[:end]
}

func assertSameSet(t *testing.T, name string, have, want []string, source string) {
	t.Helper()
	haveSet, wantSet := setOf(have), setOf(want)
	for _, id := range want {
		if !haveSet[id] {
			t.Errorf("%s is missing %q, which %s declares. Add it to internal/policyfam/nodegov/vocab.go — until you do, an org publishing that id gets a hard lint failure.", name, id, source)
		}
	}
	for _, id := range have {
		if !wantSet[id] {
			t.Errorf("%s contains %q, which %s no longer declares. Remove it from internal/policyfam/nodegov/vocab.go — a stale id lets an admin publish a directive no node can ever apply.", name, id, source)
		}
	}
}
