package copilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/tailscale/hujson"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// debugFlag is the VS Code setting that gates Copilot Chat's writing
// of substantive records (llm_request spans, tool_call spans) to the
// debug-log main.jsonl files this adapter parses. With it off, VS
// Code emits a single session_start line and nothing else, so the
// adapter sees an empty stream even when Copilot Chat is actively
// in use.
const debugFlag = "github.copilot.chat.advanced.debug"

// Logger is the subset of slog.Logger Setup uses; satisfied by *slog.Logger.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// EnsureDebugEnabled walks every cross-mount-resolved $HOME, locates
// VS Code's settings.json appropriate to the home's logical OS, and
// sets the Copilot debug flag to true when missing or false. Honors
// the user's "auto-flip but warn loudly when we do it" preference:
//
//   - When we change a file, we log INFO with the path + the prior
//     value, so the user sees exactly what observer touched.
//   - When the flag is already true, we don't log — steady state is
//     silent.
//   - When parse / write fails, we log WARN and move on; observer
//     startup is never aborted by setup failures.
//
// settings.json is JSONC (JSON-with-comments + trailing commas),
// which Go's encoding/json refuses. We use github.com/tailscale/hujson
// to roundtrip safely while preserving the user's comments and
// formatting.
func EnsureDebugEnabled(ctx context.Context, logger Logger, homes []crossmount.HomeRoot) {
	for _, h := range homes {
		if ctx.Err() != nil {
			return
		}
		path := settingsPath(h)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			// No VS Code on this home (file absent or unreadable). Not
			// an error — most cross-mount homes lack VS Code.
			continue
		}
		changed, prior, err := flipDebugFlag(path)
		if err != nil {
			logger.Warn("copilot.setup: could not enable debug flag",
				"path", path, "err", err)
			continue
		}
		if !changed {
			continue
		}
		logger.Info("copilot.setup: enabled "+debugFlag,
			"path", path, "prior_value", prior)
	}
}

// settingsPath returns the VS Code User/settings.json location under
// the given home, branching on the home's LOGICAL OS. Returns "" for
// home OSes we don't recognize.
func settingsPath(h crossmount.HomeRoot) string {
	switch h.OS {
	case crossmount.OSWindows:
		// Native Windows users may have APPDATA pointing at a roaming
		// profile redirect; respect that for the native home only.
		// Cross-mount Windows homes use the conventional layout.
		if h.Origin == "native" && runtime.GOOS == "windows" {
			if appData := os.Getenv("APPDATA"); appData != "" {
				return filepath.Join(appData, "Code", "User", "settings.json")
			}
		}
		return filepath.Join(h.Path, "AppData", "Roaming", "Code", "User", "settings.json")
	case crossmount.OSDarwin:
		return filepath.Join(h.Path, "Library", "Application Support", "Code", "User", "settings.json")
	case crossmount.OSLinux:
		return filepath.Join(h.Path, ".config", "Code", "User", "settings.json")
	}
	return ""
}

// flipDebugFlag reads path as JSONC, sets the Copilot debug flag to
// true if currently missing or false, and writes back. Returns
// (changed, priorValue, err) — priorValue is "missing" / "false" /
// "true" / "non-bool" depending on what we found.
//
// When changed=true the file was rewritten in place (atomic via a
// temp file + rename); comments and formatting in the user's
// settings.json are preserved by hujson. When changed=false the file
// is untouched.
func flipDebugFlag(path string) (changed bool, prior string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, "", fmt.Errorf("read: %w", err)
	}

	// Hujson preserves comments + trailing commas through Patch.
	// Standardize() strips them for parsing only — we never write the
	// standardized form back.
	val, err := hujson.Parse(raw)
	if err != nil {
		return false, "", fmt.Errorf("parse jsonc: %w", err)
	}

	prior, present := readBoolMember(&val, debugFlag)
	if present && prior == "true" {
		return false, prior, nil
	}

	indent := detectIndent(raw)
	if err := setOrInsertBoolMember(&val, debugFlag, true, indent); err != nil {
		return false, prior, fmt.Errorf("patch: %w", err)
	}
	out := val.Pack()

	// Atomic write: temp + rename, preserving the original mode when
	// possible.
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vscode-settings-*.tmp")
	if err != nil {
		return false, prior, fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return false, prior, fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return false, prior, fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		return false, prior, fmt.Errorf("chmod tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return false, prior, fmt.Errorf("rename: %w", err)
	}
	return true, prior, nil
}

// readBoolMember inspects the top-level object for a boolean-valued
// member by name and returns ("true"|"false"|"non-bool", true) when
// found, or ("missing", false) when absent.
func readBoolMember(v *hujson.Value, name string) (string, bool) {
	obj, ok := v.Value.(*hujson.Object)
	if !ok {
		return "missing", false
	}
	for _, m := range obj.Members {
		key, ok := m.Name.Value.(hujson.Literal)
		if !ok || key.String() != name {
			continue
		}
		lit, ok := m.Value.Value.(hujson.Literal)
		if !ok {
			return "non-bool", true
		}
		switch lit.String() {
		case "true":
			return "true", true
		case "false":
			return "false", true
		default:
			return "non-bool", true
		}
	}
	return "missing", false
}

// setOrInsertBoolMember sets name=value on the top-level object,
// inserting it as a new member at the end when absent. The indent
// argument is the leading whitespace used between members (detected
// from the input file by detectIndent so we preserve the user's
// existing tabs-vs-spaces convention without calling hujson.Format(),
// which would unilaterally re-indent the whole file). Errors only
// when the document isn't a JSONC object at the top level (very rare
// — VS Code never produces non-object settings.json).
func setOrInsertBoolMember(v *hujson.Value, name string, value bool, indent string) error {
	obj, ok := v.Value.(*hujson.Object)
	if !ok {
		return fmt.Errorf("settings.json top-level is not an object")
	}
	literal := "false"
	if value {
		literal = "true"
	}
	for i := range obj.Members {
		key, ok := obj.Members[i].Name.Value.(hujson.Literal)
		if !ok || key.String() != name {
			continue
		}
		obj.Members[i].Value.Value = hujson.Literal(literal)
		return nil
	}
	// Insert at the end. hujson.Pack emits the joining "," between
	// previous-last and our new member automatically; we own the
	// whitespace AFTER that comma (the new member's Name.BeforeExtra)
	// and the space between key and value (Value.BeforeExtra).
	// Object.AfterExtra (whitespace before the closing brace) is
	// preserved as-is so the file's tail formatting is untouched.
	obj.Members = append(obj.Members, hujson.ObjectMember{
		Name: hujson.Value{
			BeforeExtra: hujson.Extra("\n" + indent),
			Value:       hujson.Literal(`"` + name + `"`),
		},
		Value: hujson.Value{
			BeforeExtra: hujson.Extra(" "),
			Value:       hujson.Literal(literal),
		},
	})
	return nil
}

// detectIndent returns the indent string used between top-level
// members of the given JSONC source. Scans for the first newline
// followed by horizontal whitespace and a `"` and returns that
// whitespace prefix. Falls back to four spaces — the most common VS
// Code settings.json default.
func detectIndent(src []byte) string {
	m := indentDetectRE.FindSubmatch(src)
	if len(m) == 2 && len(m[1]) > 0 {
		return string(m[1])
	}
	return "    "
}

var indentDetectRE = regexp.MustCompile(`\n([ \t]+)"`)
