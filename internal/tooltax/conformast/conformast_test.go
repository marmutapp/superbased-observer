package conformast_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/tooltax/conformast"
)

// writePkg drops a throwaway Go package in a temp dir. Using a synthetic
// package rather than a real adapter keeps these unit tests from breaking
// every time an adapter's vocabulary changes — the adapters' own
// tooltax_conformance_test.go files are the integration check.
func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const sampleGo = `package sample

import "github.com/x/models"

func mapToolName(name string) string {
	switch normalize(name) {
	case "read", "readfile":
		return models.ActionReadFile
	case "bash":
		return models.ActionRunCommand
	case unusedConst:
		return models.ActionUnknown
	default:
		return models.ActionUnknown
	}
}

func unrelated(kind string) string {
	switch kind {
	case "alpha", "beta":
		return "not an action"
	}
	return ""
}

var actionMap = map[string]string{
	"Grep":  models.ActionSearchText,
	"Glob":  models.ActionSearchFiles,
	"notes": "a plain string, not an action",
}

var otherMap = map[string]string{"x": "y"}
`

const sampleTestGo = `package sample

func decoyClassifier(name string) string {
	switch name {
	case "should_never_be_extracted":
		return models.ActionReadFile
	}
	return ""
}
`

func TestSwitchCaseStringsExtractsLiteralDomain(t *testing.T) {
	dir := writePkg(t, map[string]string{"sample.go": sampleGo})
	got, err := conformast.SwitchCaseStrings(dir, "mapToolName")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "read", "readfile"}
	if len(got) != len(want) {
		t.Fatalf("got %q; want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q; want %q", got, want)
		}
	}
}

// TestSwitchCaseStringsIgnoresTestFiles pins the rule that matters most for
// honesty: the extractor must read the SHIPPING classifier, never a decoy
// that happens to sit in a _test.go beside it.
func TestSwitchCaseStringsIgnoresTestFiles(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"sample.go":      sampleGo,
		"decoy_test.go":  sampleTestGo,
		"unrelated_x.go": "package sample\n",
	})
	if _, err := conformast.SwitchCaseStrings(dir, "decoyClassifier"); err == nil {
		t.Fatal("extracted a classifier that only exists in a _test.go file")
	}
}

func TestSwitchCaseStringsErrorsOnMissingOrEmpty(t *testing.T) {
	dir := writePkg(t, map[string]string{"sample.go": sampleGo})
	if _, err := conformast.SwitchCaseStrings(dir, "noSuchFunc"); err == nil {
		t.Error("want an error for a missing function")
	}
	// unrelated() has string cases, so it is NOT empty — the empty case is
	// a function with no string labels at all.
	empty := writePkg(t, map[string]string{"e.go": "package e\nfunc f(i int) int { switch i {\ncase 1:\nreturn 2\n}\nreturn 0 }\n"})
	if _, err := conformast.SwitchCaseStrings(empty, "f"); err == nil {
		t.Error("want an error for a function with no string case labels — a silently " +
			"empty domain is the vacuous pin this package exists to prevent")
	}
}

func TestMapKeysExtractsStringKeys(t *testing.T) {
	dir := writePkg(t, map[string]string{"sample.go": sampleGo})
	got, err := conformast.MapKeys(dir, "actionMap")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Glob": true, "Grep": true, "notes": true}
	if len(got) != len(want) {
		t.Fatalf("got %q; want keys %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
	if _, err := conformast.MapKeys(dir, "noSuchVar"); err == nil {
		t.Error("want an error for a missing map var")
	}
}

// TestActionClassifierDomainFindsBothShapes pins the detector behind the
// registry's honest-zero tooth: it must find the switch AND the map, and
// must NOT report the switch/map entries whose values are ordinary strings.
func TestActionClassifierDomainFindsBothShapes(t *testing.T) {
	dir := writePkg(t, map[string]string{"sample.go": sampleGo})
	got, err := conformast.ActionClassifierDomain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["func mapToolName"]; !ok {
		t.Errorf("did not find the switch classifier; got %v", got)
	}
	if _, ok := got["var actionMap"]; !ok {
		t.Errorf("did not find the map classifier; got %v", got)
	}
	if _, ok := got["func unrelated"]; ok {
		t.Errorf("reported a switch whose cases return plain strings: %v", got)
	}
	if _, ok := got["var otherMap"]; ok {
		t.Errorf("reported a map whose values are plain strings: %v", got)
	}
	for _, n := range got["var actionMap"] {
		if n == "notes" {
			t.Error(`reported the "notes" key, whose value is not an action constant`)
		}
	}
}

// TestActionClassifierDomainEmptyForAClassifierlessPackage is the shape the
// honest-zero tooth relies on: a package that assigns action types directly
// (never switching on a tool NAME) reports no domain.
func TestActionClassifierDomainEmptyForAClassifierlessPackage(t *testing.T) {
	dir := writePkg(t, map[string]string{"b.go": `package b

import "github.com/x/models"

func build() string { return models.ActionAssistantMessage }
`})
	got, err := conformast.ActionClassifierDomain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want an empty domain", got)
	}
}
