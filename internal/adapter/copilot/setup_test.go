package copilot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// staging materializes a fake VS Code User dir tree under t.TempDir()
// so EnsureDebugEnabled has a real settings.json to read/write
// without touching the host's actual VS Code config.
func staging(t *testing.T, body string) (home string, settingsPath string) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, ".config", "Code", "User")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath = filepath.Join(dir, "settings.json")
	if body != "" {
		if err := os.WriteFile(settingsPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, settingsPath
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

func TestEnsureDebugEnabledSetsMissingFlag(t *testing.T) {
	t.Parallel()
	body := `{
    "editor.fontSize": 14,
    "files.autoSave": "afterDelay"
}
`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Use a key/value membership check rather than exact-string match
	// because hujson's Format() column-aligns colons in pretty-printed
	// output. Semantic content matters; whitespace doesn't.
	gotStr := string(got)
	if !contains(gotStr, `"github.copilot.chat.advanced.debug"`) || !contains(gotStr, "true") {
		t.Errorf("flag not added; file contents:\n%s", got)
	}
	if !contains(gotStr, `"editor.fontSize"`) || !contains(gotStr, "14") {
		t.Errorf("editor.fontSize lost:\n%s", gotStr)
	}
	if !contains(gotStr, `"files.autoSave"`) || !contains(gotStr, `"afterDelay"`) {
		t.Errorf("files.autoSave lost:\n%s", gotStr)
	}
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }

func TestEnsureDebugEnabledFlipsFalseToTrue(t *testing.T) {
	t.Parallel()
	body := `{
    "editor.fontSize": 14,
    "github.copilot.chat.advanced.debug": false,
    "files.autoSave": "afterDelay"
}
`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, _ := os.ReadFile(path)
	gotStr := string(got)
	if !strings.Contains(gotStr, `"github.copilot.chat.advanced.debug"`) {
		t.Errorf("flag key missing after flip:\n%s", gotStr)
	}
	// "false" must not appear as the flag's value. Other false literals
	// elsewhere in the file would be tolerated, but our fixture has no
	// other booleans, so a substring check is sufficient here.
	if strings.Contains(gotStr, "false") {
		t.Errorf("flag still false:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "true") {
		t.Errorf("flag not flipped to true:\n%s", gotStr)
	}
}

func TestEnsureDebugEnabledIdempotentWhenAlreadyTrue(t *testing.T) {
	t.Parallel()
	body := `{
    "github.copilot.chat.advanced.debug": true,
    "editor.fontSize": 14
}
`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	before, _ := os.ReadFile(path)
	beforeStat, _ := os.Stat(path)
	EnsureDebugEnabled(context.Background(), quietLogger(), homes)
	after, _ := os.ReadFile(path)
	afterStat, _ := os.Stat(path)

	if string(before) != string(after) {
		t.Errorf("file rewritten despite flag already true:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// Sanity: mtime should also be unchanged (write skipped).
	if !beforeStat.ModTime().Equal(afterStat.ModTime()) {
		t.Errorf("mtime changed despite no rewrite needed")
	}
}

func TestEnsureDebugEnabledPreservesCommentsAndTrailingCommas(t *testing.T) {
	t.Parallel()
	// Real-world VS Code settings.json: comments and trailing commas
	// are common. encoding/json would refuse this; hujson must
	// preserve it.
	body := `{
    // editor preferences
    "editor.fontSize": 14,
    /* multi-line
       block comment */
    "files.autoSave": "afterDelay",
}
`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, `"github.copilot.chat.advanced.debug"`) || !strings.Contains(gotStr, "true") {
		t.Errorf("flag not added in JSONC file:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "// editor preferences") {
		t.Errorf("line comment lost:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "/* multi-line") {
		t.Errorf("block comment lost:\n%s", gotStr)
	}
}

func TestEnsureDebugEnabledSkipsHomesWithoutVSCode(t *testing.T) {
	t.Parallel()
	// Home with no .config/Code/User/settings.json (e.g. a /mnt/c/Users/Public
	// dir on a WSL2 host where Public has no VS Code install).
	emptyHome := t.TempDir()
	homes := []crossmount.HomeRoot{
		{Path: emptyHome, OS: crossmount.OSLinux, Origin: "wsl-mnt:Public"},
	}

	// Must not panic / error / create files.
	EnsureDebugEnabled(context.Background(), quietLogger(), homes)
	if _, err := os.Stat(filepath.Join(emptyHome, ".config")); !os.IsNotExist(err) {
		t.Errorf("EnsureDebugEnabled created config dir on a home without VS Code")
	}
}

func TestEnsureDebugEnabledTolaratesMalformedSettings(t *testing.T) {
	t.Parallel()
	// Garbage contents: log WARN, don't abort, don't corrupt.
	body := `{ this is not json at all`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("malformed settings was rewritten:\nwant: %s\ngot: %s", body, got)
	}
}

func TestEnsureDebugEnabledPreservesFourSpaceIndent(t *testing.T) {
	t.Parallel()
	// Real VS Code default style: 4-space indent. Ensure our insert
	// follows the same indent and we don't unilaterally reformat the
	// file (which is what hujson's Format() would do).
	body := `{
    "editor.fontSize": 10,
    "files.autoSave": "afterDelay"
}
`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, _ := os.ReadFile(path)
	gotStr := string(got)
	// New member must use 4-space indent matching existing keys.
	if !strings.Contains(gotStr, "\n    \"github.copilot.chat.advanced.debug\"") {
		t.Errorf("inserted member doesn't match 4-space indent:\n%s", gotStr)
	}
	// Existing keys retain their original 4-space indent (would be
	// tabs if we'd called Format()).
	if !strings.Contains(gotStr, "\n    \"editor.fontSize\"") {
		t.Errorf("preexisting indent style changed:\n%s", gotStr)
	}
	if strings.Contains(gotStr, "\n\t\"") {
		t.Errorf("indent flipped to tabs (Format() called unintentionally?):\n%s", gotStr)
	}
}

func TestEnsureDebugEnabledPreservesTwoSpaceIndent(t *testing.T) {
	t.Parallel()
	// Some users prefer 2-space indent. detectIndent must pick it up.
	body := `{
  "editor.fontSize": 10,
  "files.autoSave": "afterDelay"
}
`
	home, path := staging(t, body)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux, Origin: "native"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "\n  \"github.copilot.chat.advanced.debug\"") {
		t.Errorf("inserted member doesn't match 2-space indent:\n%s", got)
	}
}

func TestEnsureDebugEnabledHonorsHomeOSForPath(t *testing.T) {
	t.Parallel()
	// An OSWindows home: settings.json must live under
	// AppData/Roaming/Code/User/, NOT .config/Code/User.
	home := t.TempDir()
	winDir := filepath.Join(home, "AppData", "Roaming", "Code", "User")
	if err := os.MkdirAll(winDir, 0o755); err != nil {
		t.Fatal(err)
	}
	winPath := filepath.Join(winDir, "settings.json")
	if err := os.WriteFile(winPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows, Origin: "wsl-mnt:auzy_"}}

	EnsureDebugEnabled(context.Background(), quietLogger(), homes)

	got, _ := os.ReadFile(winPath)
	gotStr := string(got)
	if !strings.Contains(gotStr, `"github.copilot.chat.advanced.debug"`) || !strings.Contains(gotStr, "true") {
		t.Errorf("Windows-side flag not set:\n%s", got)
	}
}
