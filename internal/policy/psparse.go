package policy

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
)

// PowerShell / cmd.exe dialect specifics. These live in the pure
// package (no build tags): a Linux daemon can receive Windows-client
// commands through the WSL bridge and must evaluate them identically
// to a Windows-native daemon (Windows-first arc requirement).
//
// Documented approximations, pinned by tests in psparse_test.go:
//   - Parameter matching is case-insensitive prefix matching against
//     the parameter set we model, with per-parameter minimum prefix
//     lengths chosen to mirror real-world disambiguation ("-fo" for
//     -Force because "-f" is ambiguous with -Filter; "-r" for
//     -Recurse because no other Remove-Item parameter starts with r).
//   - PowerShell here-strings (@'...'@) are not captured as payload
//     blocks; their lines tokenize as ordinary words.
//   - `powershell -Command` re-joins the remaining argv with single
//     spaces before re-lexing; original intra-argument quoting is not
//     reconstructed.

// psAliases resolves common PowerShell aliases to canonical cmdlet
// names so rule matchers see ONE base per operation regardless of how
// it was spelled. Applied only in the PowerShell dialect (a POSIX
// `rm` must stay `rm`).
var psAliases = map[string]string{
	// Remove-Item family — the rm -rf equivalents.
	"rm": "remove-item", "del": "remove-item", "erase": "remove-item",
	"rd": "remove-item", "ri": "remove-item", "rmdir": "remove-item",
	// Item manipulation.
	"ni": "new-item", "md": "new-item", "mkdir": "new-item",
	"cp": "copy-item", "copy": "copy-item", "cpi": "copy-item",
	"mv": "move-item", "move": "move-item", "mi": "move-item",
	// Content.
	"gc": "get-content", "cat": "get-content", "type": "get-content",
	"sc": "set-content", "ac": "add-content",
	// Web / execution (exfil-rule groundwork, R-17x lands post-G1).
	"iex": "invoke-expression",
	"irm": "invoke-restmethod",
	"iwr": "invoke-webrequest", "curl": "invoke-webrequest", "wget": "invoke-webrequest",
	// Registry / process (persistence vectors).
	"sp": "set-itemproperty", "saps": "start-process", "start": "start-process",
}

// psParamMatches reports whether token is the PowerShell parameter
// `full` under prefix matching: a leading '-', then a
// case-insensitive prefix of full at least minLen characters long.
func psParamMatches(token, full string, minLen int) bool {
	if len(token) < 1+minLen || token[0] != '-' {
		return false
	}
	name := strings.ToLower(token[1:])
	if len(name) > len(full) {
		return false
	}
	return strings.HasPrefix(full, name)
}

// psHasParam reports whether any argv token matches the PowerShell
// parameter under psParamMatches rules.
func psHasParam(c *Command, full string, minLen int) bool {
	for i := 1; i < len(c.Argv); i++ {
		if psParamMatches(c.Argv[i], full, minLen) {
			return true
		}
	}
	return false
}

// psCommandPayload extracts the payload of a powershell/pwsh
// invocation: -Command (prefix-matched, minimum "-c") takes the REST
// of the argv as the command text; -EncodedCommand (minimum "-e" —
// the canonical obfuscation shape `powershell -e <base64>`) takes the
// next token as base64-encoded UTF-16LE. -File payloads are ignored
// (a script file this pure package cannot read — gap F1).
func psCommandPayload(argv []string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		t := argv[i]
		if !strings.HasPrefix(t, "-") {
			continue
		}
		if psParamMatches(t, "command", 1) && i+1 < len(argv) {
			return strings.Join(argv[i+1:], " "), true
		}
		if psParamMatches(t, "encodedcommand", 1) && i+1 < len(argv) {
			if dec, ok := decodeEncodedPS(argv[i+1]); ok {
				return dec, true
			}
			return "", false
		}
	}
	return "", false
}

// decodeEncodedPS decodes a -EncodedCommand value: standard base64
// wrapping UTF-16LE text (the only encoding PowerShell accepts).
func decodeEncodedPS(b64 string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return "", false
		}
	}
	if len(raw) < 2 || len(raw)%2 != 0 {
		return "", false
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(u16)), true
}

// cmdSlashCPayload extracts the payload of a `cmd /c ...` (or /k)
// invocation: everything after the switch, re-joined, parsed under
// the cmd dialect.
func cmdSlashCPayload(argv []string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		t := strings.ToLower(argv[i])
		if (t == "/c" || t == "/k") && i+1 < len(argv) {
			return strings.Join(argv[i+1:], " "), true
		}
	}
	return "", false
}

// cmdHasFlag reports whether the cmd-dialect unit carries the given
// slash flag (case-insensitive), accepting both spaced ("/s /q") and
// run-together ("/s/q") forms.
func cmdHasFlag(c *Command, letter string) bool {
	for i := 1; i < len(c.Argv); i++ {
		t := c.Argv[i]
		if len(t) < 2 || t[0] != '/' {
			continue
		}
		for _, part := range strings.Split(t[1:], "/") {
			if strings.EqualFold(part, letter) {
				return true
			}
		}
	}
	return false
}
