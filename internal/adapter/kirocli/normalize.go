package kirocli

import (
	"encoding/json"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// toolArgs is the decoded `args` object of a Kiro tool_use. Only the
// fields the normalizer consults are typed; the rest are ignored.
type toolArgs struct {
	Command    string `json:"command"`     // fs_write: create|append|insert|str_replace ; execute_bash mirrors here too
	Path       string `json:"path"`        // fs_write / single-op fs_read
	FileText   string `json:"file_text"`   // fs_write create/append
	NewStr     string `json:"new_str"`     // fs_write str_replace/insert
	WorkingDir string `json:"working_dir"` // execute_bash
	// execute_bash carries `command` (the shell line); fs_write also
	// carries `command` (the write mode) — disambiguated by tool name.
	Operations []struct {
		Path string `json:"path"`
	} `json:"operations"` // fs_read batch
}

// normalizeTool maps a Kiro built-in / MCP tool name onto the spec §5
// normalized action taxonomy and derives a human Target + the authored
// ContentBytes. The mapping is table-driven at the switch level; the
// raw name is always preserved by the caller in RawToolName.
//
// Kiro native tools (grounded against a live conversations_v2 capture
// + the session's own `tools` spec block):
//
//	fs_read        → read_file
//	fs_write       → write_file (create/append/insert) | edit_file (str_replace)
//	execute_bash   → run_command
//	introspect     → unknown (kiro's own help/docs tool)
//	<server>___<t> → mcp_call (MCP tools are namespaced with "___")
//	(anything else)→ unknown
func normalizeTool(name string, rawArgs json.RawMessage) (action, target string, contentBytes int64) {
	var a toolArgs
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &a)
	}
	switch name {
	case "fs_read":
		return models.ActionReadFile, firstReadPath(a), 0
	case "fs_write":
		if a.Command == "str_replace" {
			return models.ActionEditFile, a.Path, int64(len(a.NewStr))
		}
		// create / append / insert all author file content.
		body := a.FileText
		if body == "" {
			body = a.NewStr
		}
		return models.ActionWriteFile, a.Path, int64(len(body))
	case "execute_bash":
		return models.ActionRunCommand, a.Command, 0
	case "introspect":
		return models.ActionUnknown, "", 0
	default:
		if strings.Contains(name, "___") {
			return models.ActionMCPCall, "", 0
		}
		return models.ActionUnknown, "", 0
	}
}

// firstReadPath returns the single-op path or the first batch-op path
// of an fs_read call.
func firstReadPath(a toolArgs) string {
	if a.Path != "" {
		return a.Path
	}
	if len(a.Operations) > 0 {
		return a.Operations[0].Path
	}
	return ""
}

// resolveProjectRoot translates a foreign-OS cwd (WSL2 reading /mnt/c,
// or a Windows observer reading \\wsl.localhost) BEFORE git.Resolve.
// The translation is UNCONDITIONAL — crossmount.TranslateForeignPath
// always converts a `C:\...` drive path to its `/mnt/c/...` absolute
// equivalent, so the drive-letter string never reaches git.Resolve
// where filepath.Abs would treat it as relative and CWD-prefix the
// observer's own .git onto it ([[feedback-foreign-path-git-resolve]]).
// Mirrors opencode/clinecli. Returns (projectRoot, gitBranch); a blank
// cwd yields ("", "").
func resolveProjectRoot(rawCWD string) (root, branch string) {
	cwd := strings.TrimSpace(rawCWD)
	if cwd == "" {
		return "", ""
	}
	cwd = crossmount.TranslateForeignPath(cwd)
	info, err := git.Resolve(cwd)
	if err != nil {
		return cwd, ""
	}
	return info.Root, info.Branch
}
