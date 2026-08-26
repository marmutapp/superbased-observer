package arena

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// claudesandbox.go — per-slot claude-code config isolation (operator ruling,
// 2026-08-22 smoke-test follow-up). The arena drives claude with
// --dangerously-skip-permissions inside worktrees that live under the arena
// workspace (~/.observer/arena by default), but the operator's real
// ~/.claude/settings.json carries hard deny rules for Edit/Write under
// ~/.observer/** — deny rules win over skip-permissions, so the first live
// candidate could not touch its own worktree (run smoke-20260822-01). The fix
// is a throwaway CLAUDE_CONFIG_DIR per slot holding a COPY of the global
// settings with exactly those conflicting deny rules filtered out; everything
// else (hooks → observer attribution, env, model prefs) is preserved verbatim.
// Mirrors benchmark_driver.go's sandbox-home precedent.

// credentialsFileName is claude-code's auth file, copied alongside settings
// when present so print-mode drives authenticate the same way as interactive
// sessions (the proxy lane forwards upstream auth).
const credentialsFileName = ".credentials.json"

// prepareClaudeSandbox creates <runDir>/.<slot>-claude-cfg populated from the
// operator's ~/.claude, returning the directory to export as CLAUDE_CONFIG_DIR.
// A missing global settings file degrades to an empty config dir (fresh
// onboarding defaults) rather than an error — the drive itself decides whether
// that works. workspaceRoot is the arena workspace whose protection must not
// leak into candidate permission rules.
func prepareClaudeSandbox(runDir, slot, workspaceRoot string) (string, error) {
	if runDir == "" || slot == "" {
		return "", fmt.Errorf("arena.prepareClaudeSandbox: runDir and slot required")
	}
	cfgDir := filepath.Join(runDir, "."+slot+"-claude-cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return "", fmt.Errorf("arena.prepareClaudeSandbox: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return cfgDir, nil // no home → nothing to copy; use bare config dir
	}
	srcSettings := filepath.Join(home, ".claude", "settings.json")
	if raw, err := os.ReadFile(srcSettings); err == nil {
		filtered, ferr := filterClaudeDenyRules(raw, home, workspaceRoot)
		if ferr != nil {
			return "", fmt.Errorf("arena.prepareClaudeSandbox: %w", ferr)
		}
		if werr := os.WriteFile(filepath.Join(cfgDir, "settings.json"), filtered, 0o600); werr != nil {
			return "", fmt.Errorf("arena.prepareClaudeSandbox: %w", werr)
		}
	}
	if creds, err := os.ReadFile(filepath.Join(home, ".claude", credentialsFileName)); err == nil {
		if werr := os.WriteFile(filepath.Join(cfgDir, credentialsFileName), creds, 0o600); werr != nil {
			return "", fmt.Errorf("arena.prepareClaudeSandbox: %w", werr)
		}
	}
	return cfgDir, nil
}

// filterClaudeDenyRules parses a settings.json document and drops
// permissions.deny entries whose path pattern covers workspaceRoot (e.g. the
// default "Edit(~/.observer/**)" / "Write(~/.observer/**)" pair vs the
// ~/.observer/arena workspace). All other content — allow/ask lists, hooks,
// env, model preferences — passes through byte-for-byte at the JSON level.
func filterClaudeDenyRules(raw []byte, home, workspaceRoot string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("filterClaudeDenyRules: %w", err)
	}
	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		return raw, nil // no permissions block → nothing to filter
	}
	denies, ok := perms["deny"].([]any)
	if !ok {
		return json.Marshal(doc)
	}
	kept := make([]any, 0, len(denies))
	for _, d := range denies {
		rule, isStr := d.(string)
		if isStr && denyRuleCovers(rule, home, workspaceRoot) {
			continue
		}
		kept = append(kept, d)
	}
	perms["deny"] = kept
	doc["permissions"] = perms
	return json.Marshal(doc)
}

// denyRuleCovers reports whether one deny rule's pattern expands to cover
// workspaceRoot. Patterns look like Verb(PATH-GLOB); only the path glob is
// examined, and a trailing /** is treated as prefix-covering so both
// "~/.observer/**" and a literal "~/.observer" rule block the arena dir.
func denyRuleCovers(rule, home, workspaceRoot string) bool {
	open := strings.IndexByte(rule, '(')
	close_ := strings.LastIndexByte(rule, ')')
	if open < 0 || close_ <= open {
		return false
	}
	pattern := strings.TrimSpace(rule[open+1 : close_])
	pattern = strings.TrimPrefix(pattern, "~")
	base := strings.TrimSuffix(filepath.Join(home, pattern), "/**")
	if base == "" || base == "/" {
		return false
	}
	return strings.HasPrefix(workspaceRoot+string(filepath.Separator), base+string(filepath.Separator)) ||
		strings.HasPrefix(base+string(filepath.Separator), workspaceRoot+string(filepath.Separator))
}
