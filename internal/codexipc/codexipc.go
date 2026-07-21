package codexipc

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"strings"
)

// Process describes a single long-running codex `app-server` instance
// that may intercept `codex exec` calls via the global IPC pipe.
type Process struct {
	// PID of the holder process.
	PID int
	// Path is the absolute executable path when known (Windows CIM
	// `Path` / POSIX `comm` field). May be empty when the OS surface
	// doesn't expose it.
	Path string
	// CommandLine is the full argv reconstruction. Used to filter
	// `app-server` from `exec` / renderer children.
	CommandLine string
	// Source classifies the holder by path heuristic so the
	// operator-facing warning can be specific. Values: "vscode-extension",
	// "codex-desktop", "unknown".
	Source string
}

// Detect enumerates long-running codex `app-server` processes on the
// current host. Returns an empty slice (not nil error) when none are
// found. The implementation is platform-specific; see detect_windows.go
// and detect_unix.go.
//
// Detect is intended to be called once per `observer codex` invocation
// and its result re-used for both the optional --exclusive termination
// pass and the post-flight capture-rate cross-reference. The cost is
// bounded — on POSIX the AF_UNIX sock is stat'd first and `ps` runs
// only when the sock is present.
func Detect(ctx context.Context) ([]Process, error) {
	return detect(ctx)
}

// Terminate kills the process with the given PID. Uses SIGKILL on
// POSIX and `taskkill /F /PID` on Windows. Returns a wrapped error
// when the operating system denies the kill (typical on Windows when
// the codex.exe process belongs to a different user / elevated
// session); callers should surface the error verbatim to the
// operator so they can fall back to manual termination.
func Terminate(ctx context.Context, pid int) error {
	return terminate(ctx, pid)
}

// parseTasklistCSV parses the output of a PowerShell CIM query of
// Win32_Process for codex.exe / Codex.exe processes. Lives in the
// platform-agnostic file so its parsing logic can be unit-tested on
// any host.
//
// The expected CSV shape (header + rows) is produced by:
//
//	Get-CimInstance Win32_Process -Filter "Name = 'codex.exe' OR Name = 'Codex.exe'" |
//	  Select-Object ProcessId,Name,Path,CommandLine |
//	  ConvertTo-Csv -NoTypeInformation
//
// Header columns: ProcessId,Name,Path,CommandLine.
//
// Filtering applied here (mirrors docs/observer-platform-issues-v5.md
// §V5-1 reproducer):
//   - CommandLine must contain "app-server"
//   - CommandLine must NOT contain "--type=" (excludes Electron
//     renderer / utility child processes)
//   - Name must contain "codex" (case-insensitive)
func parseTasklistCSV(raw []byte) []Process {
	if len(raw) == 0 {
		return nil
	}
	r := csv.NewReader(strings.NewReader(string(raw)))
	r.FieldsPerRecord = -1 // ConvertTo-Csv emits quoted fields; embedded commas in CommandLine are quoted
	header, err := r.Read()
	if err != nil {
		return nil
	}
	idxPID, idxName, idxPath, idxCmd := -1, -1, -1, -1
	for i, col := range header {
		switch strings.TrimSpace(col) {
		case "ProcessId":
			idxPID = i
		case "Name":
			idxName = i
		case "Path":
			idxPath = i
		case "CommandLine":
			idxCmd = i
		}
	}
	if idxPID < 0 || idxName < 0 || idxCmd < 0 {
		return nil
	}
	var out []Process
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if idxPID >= len(row) || idxName >= len(row) || idxCmd >= len(row) {
			continue
		}
		name := strings.ToLower(row[idxName])
		if !strings.Contains(name, "codex") {
			continue
		}
		cmdLine := row[idxCmd]
		if !strings.Contains(cmdLine, "app-server") {
			continue
		}
		if strings.Contains(cmdLine, "--type=") {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(row[idxPID]))
		if perr != nil || pid <= 0 {
			continue
		}
		path := ""
		if idxPath >= 0 && idxPath < len(row) {
			path = row[idxPath]
		}
		out = append(out, Process{
			PID:         pid,
			Path:        path,
			CommandLine: cmdLine,
			Source:      classifySource(path + " " + cmdLine),
		})
	}
	return out
}

// parsePSOutput parses the output of `ps -A -o pid,comm,args` (POSIX).
// Lives in the platform-agnostic file so its parsing logic can be
// unit-tested on any host.
//
// Filtering applied here:
//   - comm must contain "codex" (basename of the executable)
//   - args must contain "app-server"
//   - args must NOT contain "--type=" (Electron child exclusion)
//
// The ps format on Linux and macOS is the same with this `-o` spec:
// PID right-padded, COMM next, ARGS spans the rest of the line. We
// split into 3 fields max (`SplitN`) so embedded spaces in args
// survive intact.
func parsePSOutput(raw []byte) []Process {
	if len(raw) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return nil
	}
	// Header line is "  PID COMMAND          COMMAND" or similar; skip
	// the first line if it doesn't start with a digit.
	start := 0
	if len(lines) > 0 && !startsWithDigitRune(lines[0]) {
		start = 1
	}
	var out []Process
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) < 2 {
			continue
		}
		pid, perr := strconv.Atoi(fields[0])
		if perr != nil || pid <= 0 {
			continue
		}
		rest := strings.TrimSpace(fields[1])
		// rest is "comm args..."; split comm off — ps right-pads comm
		// to a fixed width, but on most modern ps's it's separated by
		// at least one space.
		commArgs := strings.SplitN(rest, " ", 2)
		if len(commArgs) < 2 {
			continue
		}
		comm := strings.TrimSpace(commArgs[0])
		args := strings.TrimSpace(commArgs[1])
		// Match: comm contains codex (handles "codex", "Codex", "codex.exe")
		if !strings.Contains(strings.ToLower(comm), "codex") {
			continue
		}
		if !strings.Contains(args, "app-server") {
			continue
		}
		if strings.Contains(args, "--type=") {
			continue
		}
		out = append(out, Process{
			PID:         pid,
			Path:        comm,
			CommandLine: args,
			Source:      classifySource(comm + " " + args),
		})
	}
	return out
}

func startsWithDigitRune(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t':
			continue
		default:
			return r >= '0' && r <= '9'
		}
	}
	return false
}

// classifySource applies path heuristics to assign a Source label so
// the operator-facing warning can name the responsible app.
//
//   - "vscode-extension" — path contains ".vscode" + "extensions" +
//     "openai.chatgpt" (the VS Code Codex extension's install root).
//   - "codex-desktop"    — path contains "Codex.app" (macOS bundle)
//     or "AppData\Local\Programs\Codex" (Windows installer drop) or
//     "/opt/Codex/" (Linux AppImage drop).
//   - "unknown"          — everything else.
//
// Match is case-insensitive on the path portion to tolerate Windows
// drive-letter casing differences.
func classifySource(probe string) string {
	low := strings.ToLower(probe)
	switch {
	case strings.Contains(low, ".vscode") && strings.Contains(low, "extensions") && strings.Contains(low, "openai.chatgpt"):
		return "vscode-extension"
	case strings.Contains(low, "codex.app"),
		strings.Contains(low, `appdata\local\programs\codex`),
		strings.Contains(low, "appdata/local/programs/codex"),
		strings.Contains(low, "/opt/codex/"):
		return "codex-desktop"
	default:
		return "unknown"
	}
}
