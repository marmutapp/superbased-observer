package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// withCodexManagedPaths points the package's managed-path probe at a temp dir
// for the duration of a test, restoring it afterward.
func withCodexManagedPaths(t *testing.T, paths ...string) {
	t.Helper()
	orig := codexManagedPaths
	codexManagedPaths = paths
	t.Cleanup(func() { codexManagedPaths = orig })
}

func TestCheckCodexTeams_NotDeployedIsOK(t *testing.T) {
	withCodexManagedPaths(t, filepath.Join(t.TempDir(), "absent.toml"))
	got := checkCodexTeams(config.Config{})
	if got.Status != StatusOK {
		t.Fatalf("no managed config should be OK, got %v: %s", got.Status, got.Message)
	}
}

func TestCheckCodexTeams_PresentParseableReports(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "requirements.toml")
	if err := os.WriteFile(p, []byte("allow_managed_hooks_only = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withCodexManagedPaths(t, p)

	got := checkCodexTeams(config.Config{})
	var sawParseable bool
	for _, d := range got.Details {
		if strings.Contains(d, "present + parseable") {
			sawParseable = true
		}
	}
	if !sawParseable {
		t.Fatalf("expected a 'present + parseable' detail, got %+v", got.Details)
	}
}

func TestCheckCodexTeams_BadTOMLWarns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "managed_config.toml")
	if err := os.WriteFile(p, []byte("this is = = not valid toml ]["), 0o644); err != nil {
		t.Fatal(err)
	}
	withCodexManagedPaths(t, p)

	got := checkCodexTeams(config.Config{})
	if got.Status != StatusWarn {
		t.Fatalf("invalid TOML should WARN, got %v", got.Status)
	}
	var sawBad bool
	for _, d := range got.Details {
		if strings.Contains(d, "not valid TOML") {
			sawBad = true
		}
	}
	if !sawBad {
		t.Fatalf("expected a 'not valid TOML' detail, got %+v", got.Details)
	}
}
