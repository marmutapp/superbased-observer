package hook

import (
	"path/filepath"
	"strings"
)

// RewriteBash wraps a Bash command in `observer run --` when doing so is
// safe and likely to help. Returns (newCommand, true) on rewrite, or
// (original, false) when the command should pass through unchanged.
//
// Rewriting is skipped when:
//   - the command is empty
//   - the binary path is empty (nothing to prepend)
//   - the command contains unquoted shell operators (|, &, ;, <, >, (, ), `, $)
//     — those require the host shell's own interpretation, which `observer
//     run` bypasses
//   - the first word's basename is in excludeCommands
//   - the first word is already `observer` (double-wrap guard)
//
// The rewrite form is `<binary> run -- <original>`. The `--` is intentional
// so that flags embedded in the wrapped command (e.g. `git --no-pager log`)
// are never consumed by `observer run` itself.
func RewriteBash(binary, original string, excludeCommands []string) (string, bool) {
	trimmed := strings.TrimSpace(original)
	if trimmed == "" || binary == "" {
		return original, false
	}
	if !isShellSimple(trimmed) {
		return original, false
	}
	first := firstWord(trimmed)
	if first == "" {
		return original, false
	}
	base := filepath.Base(first)
	if base == "observer" {
		return original, false
	}
	for _, ex := range excludeCommands {
		if ex == base {
			return original, false
		}
	}
	return binary + " run -- " + trimmed, true
}

// isShellSimple returns true when cmd contains no shell operators that
// require the host shell to interpret. A small state machine tracks
// single-quote, double-quote, and backslash escaping:
//
//   - outside quotes: any of | & ; < > ( ) ` $ disqualifies
//   - inside double quotes: $ and ` are still interpreted (so they disqualify)
//   - inside single quotes: everything is literal — no metachar checks
//
// Unbalanced quotes are treated as non-simple since the host shell would not
// execute them the same way argv-passing does.
func isShellSimple(cmd string) bool {
	var (
		inSingle bool
		inDouble bool
		escape   bool
	)
	for _, r := range cmd {
		if escape {
			escape = false
			continue
		}
		switch {
		case r == '\\' && !inSingle:
			escape = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case inDouble:
			// Inside double quotes, only $ and ` still trigger shell
			// interpretation.
			if r == '$' || r == '`' {
				return false
			}
		case !inSingle && !inDouble:
			switch r {
			case '|', '&', ';', '<', '>', '(', ')', '`', '$':
				return false
			}
		}
	}
	return !inSingle && !inDouble
}

// firstWord returns the first whitespace-separated token of cmd, stripped.
// Quoting inside the first word is preserved; we only care about the token
// boundary for command-name classification.
func firstWord(cmd string) string {
	s := strings.TrimSpace(cmd)
	for i, r := range s {
		if r == ' ' || r == '\t' {
			return s[:i]
		}
	}
	return s
}
